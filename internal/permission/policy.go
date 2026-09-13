// Package permission classifies shell commands and routes "ask" commands
// through a user approval flow.
package permission

import (
	"fmt"
	"strings"
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

// Policy classifies shell commands. Rules are data (sets + patterns), not
// hardcoded branches in the executor.
type Policy struct {
	safePrograms    map[string]bool
	askPrograms     map[string]bool
	blockedPrograms map[string]bool
	// dangerousSubstrings refuse a command regardless of program.
	dangerousSubstrings []string
}

func DefaultPolicy() *Policy {
	return &Policy{
		safePrograms: toSet([]string{
			"git", "go", "gofmt", "cmake", "make", "ninja", "ctest", "gcc", "g++", "clang",
			"rg", "grep", "find", "cat", "ls", "pwd", "head", "tail", "wc", "sort", "uniq",
			"diff", "echo", "printf", "touch", "mkdir", "cp", "mv",
			"python", "python3", "pip", "node", "npm", "npx", "yarn", "pnpm",
			"cargo", "rustc", "javac", "java", "mvn", "gradle",
			"true", "false", "test", "env", "which", "sleep",
			"exit", "cd", "export", "unset", "set", "source", "xargs", "tee",
		}),
		askPrograms: toSet([]string{
			"docker", "podman", "kubectl", "ssh", "scp", "rsync", "sftp",
			"curl", "wget", "apt", "apt-get", "dnf", "yum", "brew", "snap",
			"systemctl", "service", "rm", "kill", "pkill",
		}),
		blockedPrograms: toSet([]string{
			"sudo", "su", "doas", "shutdown", "reboot", "halt", "poweroff",
			"mkfs", "fdisk", "parted", "dd", "mount", "umount",
			"useradd", "userdel", "passwd", "chroot",
		}),
		dangerousSubstrings: []string{
			"rm -rf /", "rm -rf /*", "rm -fr /",
			":(){ :|:& };:", // fork bomb
			"> /dev/sda", "of=/dev/",
			"chmod -r 777 /", "chmod -R 777 /",
			"mkfs.", "dd if=",
		},
	}
}

// NewPolicy builds a policy from explicit program lists (for embedding or
// tests). Unknown programs default to Ask.
func NewPolicy(safe, ask, blocked, dangerous []string) *Policy {
	return &Policy{
		safePrograms:        toSet(safe),
		askPrograms:         toSet(ask),
		blockedPrograms:     toSet(blocked),
		dangerousSubstrings: dangerous,
	}
}

// Classify returns the verdict for a full shell command line and a
// human-readable reason. Compound commands (pipes, &&, ;, ||) are split and
// every segment is classified; the strictest verdict wins.
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

	decision := Safe
	reason := "all programs are in the safe list"
	for _, segment := range splitCompound(trimmed) {
		prog := firstProgram(segment)
		if prog == "" {
			continue
		}
		switch {
		case p.blockedPrograms[prog]:
			return Blocked, fmt.Sprintf("program %q is blocked", prog)
		case p.askPrograms[prog]:
			if decision < Ask {
				decision = Ask
				reason = fmt.Sprintf("program %q requires approval", prog)
			}
		case p.safePrograms[prog]:
			// still safe
		default:
			if decision < Ask {
				decision = Ask
				reason = fmt.Sprintf("unknown program %q requires approval", prog)
			}
		}
	}
	return decision, reason
}

// splitCompound splits on shell control operators (&&, ||, ;, |). Single &
// (background) is not a split point; redirections are handled in
// firstProgram. Quotes are not fully parsed — that only ever makes
// classification stricter, never looser.
func splitCompound(cmd string) []string {
	var segments []string
	var cur strings.Builder
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case c == ';':
			segments = append(segments, cur.String())
			cur.Reset()
		case c == '|':
			segments = append(segments, cur.String())
			cur.Reset()
			if i+1 < len(cmd) && cmd[i+1] == '|' {
				i++
			}
		case c == '&' && i+1 < len(cmd) && cmd[i+1] == '&':
			segments = append(segments, cur.String())
			cur.Reset()
			i++
		default:
			cur.WriteByte(c)
		}
	}
	return append(segments, cur.String())
}

// firstProgram extracts the executable name from a segment, skipping
// leading VAR=value assignments and redirections (>, >>, 2>, 2>&1, ...).
func firstProgram(segment string) string {
	fields := strings.Fields(segment)
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if isRedirection(f) {
			if f == ">" || f == ">>" || f == "<" {
				i++ // the target is a separate token
			}
			continue
		}
		if strings.Contains(f, "=") && !strings.HasPrefix(f, "-") {
			name := f[:strings.Index(f, "=")]
			if name != "" && isEnvName(name) {
				continue
			}
		}
		// strip any path prefix: /usr/bin/git -> git
		if i2 := strings.LastIndex(f, "/"); i2 >= 0 {
			f = f[i2+1:]
		}
		return strings.ToLower(f)
	}
	return ""
}

// isRedirection reports whether a token is a shell redirection operator,
// e.g. ">", ">>", "<", "2>", "2>>", ">&2", "2>&1".
func isRedirection(tok string) bool {
	if strings.HasPrefix(tok, ">") || strings.HasPrefix(tok, "<") {
		return true
	}
	if len(tok) >= 2 && tok[0] >= '0' && tok[0] <= '9' && (tok[1] == '>' || tok[1] == '<') {
		return true
	}
	return false
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
