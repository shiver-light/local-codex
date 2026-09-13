// Package permission classifies shell commands and routes "ask" commands
// through a user approval flow.
package permission

import (
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Decision is the policy verdict for a shell command.
type Decision int

const (
	// Safe commands run without user interaction.
	Safe Decision = iota
	// Ask commands require explicit user approval.
	Ask
	// Blocked commands are always refused.
	Blocked
)

func (d Decision) String() string {
	switch d {
	case Safe:
		return "safe"
	case Ask:
		return "ask"
	default:
		return "blocked"
	}
}

// Policy classifies shell commands. Rules are data (sets + subcommand
// tables), not hardcoded branches in the executor.
type Policy struct {
	safePrograms    map[string]bool
	askPrograms     map[string]bool
	blockedPrograms map[string]bool
	// dangerousSubstrings refuse a command regardless of program. Kept only
	// for patterns that cannot be expressed over the parsed argv (fork bomb,
	// raw device writes); rm/chmod-style rules are argv-based below.
	dangerousSubstrings []string
	// readOnlyGitSubcommands may run without approval; any other git
	// subcommand (push, clean, reset, branch -D, ...) requires approval.
	readOnlyGitSubcommands map[string]bool
	// safeGoSubcommands may run without approval.
	//
	// Tradeoff: `go build` / `go test` / `go vet` can execute arbitrary code
	// (test binaries, cgo, generated mains). They stay safe because running
	// builds and tests is the core purpose of a local coding agent; code
	// execution inside the workspace is an accepted risk. `go run`,
	// `go generate`, `go install`, `go mod`, ... require approval.
	safeGoSubcommands map[string]bool
}

func defaultReadOnlyGitSubcommands() map[string]bool {
	return toSet([]string{
		"status", "diff", "log", "show", "blame", "annotate", "grep",
		"ls-files", "ls-tree", "rev-parse", "describe", "shortlog",
		"reflog", "whatchanged",
	})
}

func defaultSafeGoSubcommands() map[string]bool {
	return toSet([]string{
		"build", "test", "vet", "list", "env", "version", "doc", "fmt",
	})
}

func DefaultPolicy() *Policy {
	return &Policy{
		// Pure read-only commands. Interpreters (python, node), package
		// managers and build tools (make, npm, pip, cargo) are deliberately
		// NOT safe: they execute arbitrary code and require approval.
		safePrograms: toSet([]string{
			"ls", "cat", "head", "tail", "wc", "rg", "grep", "find",
			"pwd", "sort", "uniq", "diff", "echo", "printf",
			"true", "false", "test", "which", "gofmt",
		}),
		askPrograms: toSet([]string{
			// Interpreters, build tools, package managers.
			"python", "python3", "pip", "pip3", "node", "npm", "npx",
			"yarn", "pnpm", "cargo", "rustc", "java", "javac", "mvn",
			"gradle", "make", "cmake", "ninja", "ctest", "gcc", "g++",
			"clang", "sh", "bash", "zsh",
			// Wrappers whose inner command cannot be trusted statically.
			"xargs", "time", "nice", "nohup", "env",
			// Filesystem writers.
			"touch", "mkdir", "cp", "mv", "ln", "tee", "chmod", "chown",
			"tar", "zip", "unzip", "gzip", "rm",
			// Network / system / container tools.
			"docker", "podman", "kubectl", "ssh", "scp", "rsync", "sftp",
			"curl", "wget", "apt", "apt-get", "dnf", "yum", "brew", "snap",
			"systemctl", "service", "kill", "pkill",
		}),
		blockedPrograms: toSet([]string{
			"sudo", "su", "doas", "shutdown", "reboot", "halt", "poweroff",
			"mkfs", "fdisk", "parted", "dd", "mount", "umount",
			"useradd", "userdel", "passwd", "chroot",
		}),
		dangerousSubstrings: []string{
			":(){ :|:& };:", // fork bomb
			"> /dev/sda", "of=/dev/",
			"chmod -r 777 /", "chmod -R 777 /",
			"mkfs.", "dd if=",
		},
		readOnlyGitSubcommands: defaultReadOnlyGitSubcommands(),
		safeGoSubcommands:      defaultSafeGoSubcommands(),
	}
}

// NewPolicy builds a policy from explicit program lists (for embedding or
// tests). Unknown programs default to Ask. git and go keep the default
// subcommand tables.
func NewPolicy(safe, ask, blocked, dangerous []string) *Policy {
	return &Policy{
		safePrograms:           toSet(safe),
		askPrograms:            toSet(ask),
		blockedPrograms:        toSet(blocked),
		dangerousSubstrings:    dangerous,
		readOnlyGitSubcommands: defaultReadOnlyGitSubcommands(),
		safeGoSubcommands:      defaultSafeGoSubcommands(),
	}
}

// Classify returns the verdict for a full shell command line and a
// human-readable reason. The command is parsed into a syntax tree and every
// command that would execute is classified — including pipelines, compound
// statements, multi-line scripts, command substitutions and commands wrapped
// by env/nice/nohup. The strictest verdict wins. Anything the parser cannot
// handle (syntax errors, non-literal expansions) degrades to Ask: better to
// ask too often than to let something through.
func (p *Policy) Classify(command string) (Decision, string) {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return Blocked, "empty command"
	}
	lower := strings.ToLower(trimmed)
	for _, pat := range p.dangerousSubstrings {
		if strings.Contains(lower, pat) {
			return Blocked, fmt.Sprintf("matches dangerous pattern %q", pat)
		}
	}

	f, err := syntax.NewParser().Parse(strings.NewReader(trimmed), "")
	if err != nil {
		return Ask, fmt.Sprintf("cannot parse shell syntax (%v); approval required", err)
	}

	c := &classifier{p: p, decision: Safe, reason: "all commands are read-only"}
	syntax.Walk(f, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.CallExpr:
			c.classifyCall(n)
		case *syntax.Redirect:
			c.checkRedirect(n)
		}
		return c.decision != Blocked
	})
	return c.decision, c.reason
}

type classifier struct {
	p        *Policy
	decision Decision
	reason   string
}

func (c *classifier) escalate(d Decision, reason string) {
	if d > c.decision {
		c.decision = d
		c.reason = reason
	}
}

// classifyCall classifies one command invocation (CallExpr). Command
// substitutions inside its words are visited by Walk on their own.
func (c *classifier) classifyCall(ce *syntax.CallExpr) {
	if len(ce.Args) == 0 {
		// Pure assignment (e.g. `FOO=bar`); command substitutions in the
		// values are visited separately by Walk.
		return
	}
	args := make([]string, 0, len(ce.Args))
	for _, w := range ce.Args {
		s, ok := literalWord(w)
		if !ok {
			c.escalate(Ask, "command contains expansions that cannot be resolved statically")
			return
		}
		args = append(args, s)
	}

	// Unwrap wrappers (env, nice, nohup, command, builtin) so the wrapped
	// command is classified instead of the wrapper.
	for len(args) > 0 {
		rest, wrapped := unwrapWrapper(baseName(args[0]), args[1:])
		if !wrapped {
			break
		}
		args = rest
	}
	if len(args) == 0 {
		c.escalate(Ask, "wrapper without a resolvable inner command")
		return
	}

	prog := baseName(args[0])
	argv := args[1:]

	if c.p.blockedPrograms[prog] || strings.HasPrefix(prog, "mkfs.") {
		c.escalate(Blocked, fmt.Sprintf("program %q is blocked", prog))
		return
	}

	switch prog {
	case "rm":
		if isDestructiveRm(argv) {
			c.escalate(Blocked, "recursive force rm with a dangerous target is blocked")
			return
		}
	case "find":
		if findModifies(argv) {
			c.escalate(Ask, "find with -delete/-exec requires approval")
			return
		}
	case "git":
		d, r := c.p.classifyGit(argv)
		c.escalate(d, r)
		return
	case "go":
		d, r := c.p.classifyGo(argv)
		c.escalate(d, r)
		return
	}

	switch {
	case c.p.askPrograms[prog]:
		c.escalate(Ask, fmt.Sprintf("program %q requires approval", prog))
	case c.p.safePrograms[prog]:
		// still safe
	default:
		c.escalate(Ask, fmt.Sprintf("unknown program %q requires approval", prog))
	}
}

// checkRedirect refuses writes to raw devices and treats non-literal
// redirection targets as unresolvable.
func (c *classifier) checkRedirect(r *syntax.Redirect) {
	if r.Word == nil {
		return
	}
	target, ok := literalWord(r.Word)
	if !ok {
		c.escalate(Ask, "redirection target cannot be resolved statically")
		return
	}
	if strings.HasPrefix(target, "/dev/") {
		c.escalate(Blocked, fmt.Sprintf("redirection to device %q is blocked", target))
	}
}

// literalWord renders a parsed word to a string only if it consists purely
// of literal (quoted or unquoted) text. Parameter expansions, command
// substitutions, arithmetic, process substitution and extended globs make
// the word unresolvable.
func literalWord(w *syntax.Word) (string, bool) {
	var sb strings.Builder
	for _, part := range w.Parts {
		switch pt := part.(type) {
		case *syntax.Lit:
			sb.WriteString(pt.Value)
		case *syntax.SglQuoted:
			sb.WriteString(pt.Value)
		case *syntax.DblQuoted:
			for _, inner := range pt.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return "", false
				}
				sb.WriteString(lit.Value)
			}
		default:
			return "", false
		}
	}
	return sb.String(), true
}

// unwrapWrapper strips commands that only wrap another command, returning
// the wrapped command's argv. wrapped reports whether prog is a known
// wrapper. xargs is deliberately not unwrapped: it synthesizes argv for the
// inner command, so static classification is unreliable — it stays Ask.
func unwrapWrapper(prog string, rest []string) (argv []string, wrapped bool) {
	skip := 0
	switch prog {
	case "env":
		for skip < len(rest) {
			a := rest[skip]
			switch {
			case a == "-u" || a == "--unset" || a == "-C" || a == "--chdir" || a == "-S" || a == "--split-string":
				skip += 2
			case strings.HasPrefix(a, "-"):
				skip++
			case isAssignment(a):
				skip++
			default:
				return rest[skip:], true
			}
		}
		return nil, true
	case "nohup", "command", "builtin":
		return rest, true
	}
	return nil, false
}

// isDestructiveRm reports whether argv is a recursive force delete with a
// dangerous target. The check runs on parsed argv, so whitespace and flag
// variants (`rm  -rf  /`, `rm -r -f /`, `rm --recursive --force ~`) cannot
// bypass it. Any target containing `/`, `~` or `*` is treated as dangerous.
func isDestructiveRm(args []string) bool {
	recursive, force := false, false
	endOfFlags := false
	var targets []string
	for _, a := range args {
		if !endOfFlags && a == "--" {
			endOfFlags = true
			continue
		}
		if !endOfFlags && len(a) > 1 && strings.HasPrefix(a, "-") {
			switch {
			case a == "--recursive":
				recursive = true
			case a == "--force":
				force = true
			case strings.HasPrefix(a, "--"):
				// other long option, no target
			default:
				for _, f := range a[1:] {
					switch f {
					case 'r', 'R':
						recursive = true
					case 'f':
						force = true
					}
				}
			}
			continue
		}
		targets = append(targets, a)
	}
	if !recursive || !force {
		return false
	}
	for _, t := range targets {
		if strings.ContainsAny(t, "/~*") {
			return true
		}
	}
	return false
}

// findModifies reports whether find would delete files or run commands.
func findModifies(args []string) bool {
	for _, a := range args {
		switch a {
		case "-delete", "-exec", "-execdir", "-ok", "-okdir":
			return true
		}
	}
	return false
}

// classifyGit resolves the git subcommand (skipping global flags like
// `-C dir` and `-c key=val`) and allows only read-only subcommands.
func (p *Policy) classifyGit(args []string) (Decision, string) {
	sub := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace":
			i++ // these take a separate value
		case strings.HasPrefix(a, "-"):
			// flag without a separate value
		default:
			sub = a
			i = len(args)
		}
	}
	if sub == "" {
		return Safe, "bare git invocation is read-only"
	}
	if p.readOnlyGitSubcommands[sub] {
		return Safe, fmt.Sprintf("git %s is read-only", sub)
	}
	return Ask, fmt.Sprintf("git subcommand %q requires approval", sub)
}

// classifyGo allows only build/verification subcommands; `go run`,
// `go generate`, `go install`, `go mod` and friends require approval.
func (p *Policy) classifyGo(args []string) (Decision, string) {
	sub := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			sub = a
			break
		}
	}
	if sub == "" {
		return Safe, "bare go invocation is read-only"
	}
	if sub == "env" {
		for _, a := range args {
			if a == "-w" || a == "-u" { // writes the go env config file
				return Ask, "go env -w/-u modifies configuration and requires approval"
			}
		}
	}
	if p.safeGoSubcommands[sub] {
		return Safe, fmt.Sprintf("go %s is allowed", sub)
	}
	return Ask, fmt.Sprintf("go subcommand %q requires approval", sub)
}

func baseName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		path = path[i+1:]
	}
	return strings.ToLower(path)
}

func isAssignment(s string) bool {
	i := strings.Index(s, "=")
	if i <= 0 {
		return false
	}
	return isEnvName(s[:i])
}

func isEnvName(s string) bool {
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

func toSet(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}
