# History: abstraction-provider-lemonade

Linked from [CONTRACT.md](CONTRACT.md)'s "Reading this page."

## Superseded ids

None. Every `LEMONADE-*` id was declared on 2026-10-02 and none has been
retired.

## The first contract page, 2026-10-02

The architecture review of 2026-10-02
(`research/architecture-review-2026-10-02.md`, "Coherence" and release
blocker 5) found the provider Setup installs as the beginner engine with
only `packaging/README.md`. Branch `docs-match-tree` wrote README.md and
CONTRACT.md from the code in `go/` and the packaging page. The rules state
what the code did that day; no behaviour changed with them.

| ids | topic |
| --- | --- |
| LEMONADE-A1, A2 | admission: one profile table, then staged audio, backends, the launch and the audio route in that order |
| LEMONADE-P1..P4 | the pinned package table, the engine-file proof, Setup's verbs, weight formats |
| LEMONADE-L1..L5 | the contained tree, the clean environment, the locked configuration, the loopback check, the idle stop after audio |
| LEMONADE-I1, I2 | the image routes and the checked request form |
| LEMONADE-C1 | the chat route's five llama.cpp b10825 conditions |
| LEMONADE-S1..S3 | the audio routes, the provider's own audio bytes, no resubmission after a speech failure |
| LEMONADE-R1 | the runtime as the only caller |
| LEMONADE-E1 | the typed admission refusals |

The page holds no H rule yet. The card lease and the `gpufault` Guard
arrive with branch `lemonade-card-lease` (review finding D1); until then
the page says `lemonaded` takes no lease.

## Design records

- [packaging/README.md](packaging/README.md): the installer feature, the
  version table, Setup's verbs and every security review condition.
- `research/lemonade-updates/DECISION.md`: versions and updates.
- `research/security-reviews/`: the Lemonade 2026.39.1, stable-diffusion.cpp
  master-843-462d675, llama.cpp b10825, whisper.cpp v1.8.4 and Kokoros b17
  reviews.
