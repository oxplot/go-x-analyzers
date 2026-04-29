# go-x-analyzers

`go-x-analyzers` is a standalone `multichecker` binary for analyzers in
`golang.org/x/tools/go/analysis/passes`.

The generated `main.go` is derived from `golang.org/x/tools`. Running
`go generate ./...` first advances that module to `@latest`, then scans direct
pass packages for exported `Analyzer` values and exported analyzer `Suite`
values.

```sh
go generate ./...
```

Build and run the checker:

```sh
go build -o go-x-analyzers .
./go-x-analyzers ./...
```
