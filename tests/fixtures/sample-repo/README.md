# sample-repo

A tiny Go calculator module used to test local-codex end-to-end.

## Known bug

`calculator.Sub` is broken: it **adds** its operands instead of subtracting
them. `TestSub` fails because of this. A coding agent should locate the bug
in `calculator/calculator.go`, fix `Sub` to return `a - b`, and verify with
`go test ./...`.
