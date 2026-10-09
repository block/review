# AGENTS.md

This is a public open source repository. Everything committed here, including commit messages, author metadata, and pull request text, is visible to everyone.

## Keep internal details out

Never include employer- or deployment-specific details in code, docs, tests, examples, commit messages, or PR descriptions:

- Internal hostnames, URLs, gateways, or service names.
- Internal model or serving-endpoint names. Use public model IDs or placeholders such as `model-a`.
- Internal ticket IDs, issue trackers, chat channels, or agent thread links.
- Internal repository names, tools, people, teams, or file paths from a developer's machine.
- Secrets, tokens, or credentials, even short-lived or example ones.

Keep commits free of trailers that link to private systems. Author commits with a public or noreply email address.

## Development

```sh
go build ./...
go vet ./...
go test ./...
```

The module uses only the Go standard library. Do not add dependencies without a strong reason.
