# Upstream protobuf schema

`iterm2/api.proto` is an unmodified copy of the iTerm2 API schema.

| | |
|---|---|
| Repository | https://github.com/gnachman/iTerm2 |
| Path | `proto/api.proto` |
| Commit | `fe8cbff71a07035ac317c0f6535c48aab77d5e76` (2026-10-12) |
| Server protocol version at that commit | 1.23 (`X-iTerm2-Protocol-Version`) |
| SHA-256 | `ebc87c0661274279f8d0db0e766f4ac2df83a7e3bb22f42991847e6da8d4c94d` |

The file lives under an `iterm2/` directory so that it registers in the Go
protobuf registry as `iterm2/api.proto` rather than the collision-prone
`api.proto`. Its contents are byte-for-byte identical to upstream.

The schema contains no `go_package` option. The Go import path is supplied at
generation time with `--go_opt=M...` (see the repository `Makefile`), so the
upstream file never needs editing.

## Updating

```sh
make proto-fetch COMMIT=<sha>   # downloads, prints the new sha256
make proto                      # regenerates internal/iterm2pb/api.pb.go
```

Then update the commit, protocol version, and SHA-256 above.

The schema is proto2 and has evolved additively; a newer schema talks to older
iTerm2 releases (unknown fields are ignored on both sides).
