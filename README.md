# review

An LLM code reviewer for git changes, built on [Goose](https://github.com/aaif-goose/goose). Every review runs the same pipeline:

```
  Behavior &        Reliability &       Security &
  Contracts         Operations          Trust Boundaries      three panelists, in parallel
      └─────────────────┼─────────────────┘
                        ▼
                   Coordinator                                dedupe, then try to disprove each candidate
                  kept │  │ dropped, with reasons
                       │  ▼
                       │ Judge                                restore drops that should have been kept
                       ▼  ▼
                     Deliver                                  merge, dedupe, gate
```

- **Panelists** each review the whole change through one fixed lens and report candidates with evidence, priority (P0–P3), and an honest confidence.
- **The coordinator** merges duplicates and tries to disprove every candidate against the code. It drops a candidate as disproved only with concrete counter-evidence, and records every drop with its reason.
- **The judge** re-examines only the dropped candidates and restores the ones that are real defects. Use a different model than the coordinator, so the two don't share blind spots.
- **Delivery** merges the coordinator's and judge's findings and applies a gate: by default P0–P2 with confidence of at least 0.8. Calibrate the gate per model; models differ in how they rate confidence.

If a panelist fails, the coordinator covers its lens and the review is marked degraded. If the judge fails, the coordinator's findings still stand.

## Usage

```sh
go install github.com/block/review/cmd/review@latest

# Review the current branch against main with Goose's configured provider and model.
review review --base main

# A different model per pass, every finding regardless of the gate, as JSON.
review review --base main --provider openai \
  --role-models behavior_state_data=model-a,failure_concurrency_lifecycle=model-b,security_contracts=model-c,coordinator=model-a,judge=model-b \
  --role-efforts behavior_state_data=medium,coordinator=medium \
  --all --json
```

The review covers `merge-base(base, head)..head`. Pass the change description with `--intent`; reviewers treat it as untrusted evidence, not instructions. Goose reads provider settings from the environment, for example `OPENAI_API_KEY`.

Each pass runs `goose run` with only the developer extension, in a fresh Goose home, so no user config, extensions, hints, or sessions leak into the review.

### Sandboxing

Goose has no read-only sandbox, and reviewers run shell commands. Run reviews in a disposable container or another sandbox that confines the checkout and the network.

## As a library

```go
res, err := review.Run(ctx, review.Config{
	RepoDir: ".",
	BaseSHA: "main",
	Goose:   review.GooseConfig{Provider: "openai", Model: "gpt-5.6-sol", Effort: "high"},
}, review.Options{})
```

`review.Options.Runner` replaces how each pass executes, and `OnPass` reports every pass, so a host can add its own accounting, routing, or telemetry.

A host that stages its own Goose config and reads usage from Goose's session store can set `GooseConfig.SharedHome`, so every pass runs in the caller's Goose home and records its session there.

## ReviewBench

The image implements the [ReviewBench agent contract](https://github.com/review-bench/ReviewBench/blob/main/AGENT_CONTRACT.md). Each `v*` tag publishes `ghcr.io/block/review:<tag>`, and the release workflow's summary lists the `ghcr.io/block/review@sha256:…` digest to register. From a ReviewBench checkout:

```sh
scripts/try-agent.sh ghcr.io/block/review@sha256:<digest> --pr 0 -e OPENAI_API_KEY

# Or a local build:
docker build --platform linux/amd64 -t review .
scripts/try-agent.sh review --pr 0 -e OPENAI_API_KEY
```

| Setting | Values | Default |
|---|---|---|
| `RB_CONFIG_PROVIDER` | Goose provider | `openai` |
| `RB_CONFIG_MODEL` | model id | `gpt-5.6-sol` |
| `RB_CONFIG_EFFORT` | `low`, `medium`, `high` | `high` |
| `RB_CONFIG_ROLE_MODELS` | `role=model,...` | none |
| `RB_CONFIG_ROLE_EFFORTS` | `role=effort,...` | none |
| `RB_CONFIG_GATE` | `on`, `off` | `on` |

With the `openai` provider, the model endpoint comes from `RB_MODEL_BASE_URL` and the key from `OPENAI_API_KEY`. The whole review is budgeted to fit ReviewBench's 15-minute limit per pull request: up to 7 minutes for the panel, then the coordinator and judge share the rest of a 14-minute budget.

## License

Apache License 2.0. See [LICENSE](LICENSE).
