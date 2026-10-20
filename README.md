# abstraction-provider-lemonade

*For operators and provider authors.*

This module builds `lemonaded`, the provider the beginner setup installs for local
pictures. It runs the pinned portable Lemonade app as its own child process
and serves OA's native inference contracts through it. The installer
feature, the package records and every security condition are in
[packaging/README.md](packaging/README.md); the rules are in
[CONTRACT.md](CONTRACT.md).

**What's new here:** The beginner's local picture, transcription and speech
engine runs pinned builds proved file by file before every launch, starts when
a request needs it, and checks each request before the engine sees it.

## What it provides

| Service | Engine Lemonade starts | Models |
| --- | --- | --- |
| `abstraction.inference/image@1` | stable-diffusion.cpp `sd-server` master-843-462d675, Vulkan | FLUX.2 [klein] 4B (`user.LocalApprovedKlein`, the starter model, with edits); `lemonaded model add` adds Z-Image-Turbo or Qwen-Image-2512 |
| `abstraction.inference/transcription@1` | whisper.cpp v1.8.4 `whisper-server`, Vulkan | `Whisper-Large-v3-Turbo` |
| `abstraction.inference/speech@1` | Kokoros b17 `koko` | `kokoro-v1` |
| chat | llama.cpp b10825 `llama-server`, Vulkan | none yet |

The chat route through llama-server is built and guarded, and stays closed
to OA callers until Setup offers a reviewed chat model with a pinned SHA-256
and the provider declares it. The provider also serves
`abstraction.storage/content-reader@1` and `content-writer@1` on its
endpoint for the runtime alone: it publishes each result into its private
store, and the runtime copies verified bytes into the store the application
is allowed to read.

## How it is installed

1. The Windows installer's optional `LemonadeImage` feature (payload
   feature `lemonadeimage`, off by default) places `tools\lemonaded.exe` and
   its declaration `tools\declarations\lemonade-image.json`. Installing the
   feature starts no engine and downloads no model.
2. Setup in the Panel installs a pinned Lemonade portable ZIP (2026.39.1
   today; `server.Packages` lists every version this build runs) under `%LOCALAPPDATA%\OpenAbstractions\apps\lemonade\<version>\` and
   downloads the reviewed backends and model files through OA download jobs,
   each after the person agrees.
3. Setup then calls `lemonaded` with fixed verbs, and nothing else calls
   them:
   - `configure --activate-reviewed-package` stages the backends and moves
     the model files into the provider's private state by rename;
     `--speech-only` and `--engine-files-only` stage less.
   - `package check`, `package activate` and `package retire` move the
     selection from one Lemonade version to the next.
   - `model add --id MODEL` adds one more picture model.
   - `setup-info --json`, `setup-probe --json` and `verify-setup --json`
     report state without starting Lemonade.
   - `engine unlock` gives the read-only engine folders back to the account;
     the uninstaller runs it before `lemonaded` goes.

Every command takes `--state DIR` and `--dir DIR` for an isolated install.
Run `lemonaded package --help`, `lemonaded model --help` or
`lemonaded configure -h` for the options.

## The engines it starts

`lemonaded` starts one Lemonade process tree on the first admitted request
and runs `lemond.exe` from the selected, proven package folder. Lemonade
starts `sd-server`, `llama-server`, `whisper-server` and `koko` from the
backend folders the provider staged and proved. Each launch proves every
file against its pin, holds the files until the tree exits, builds the
environment from nothing, passes two per-launch keys, and checks that the
tree holds only loopback sockets. The resource holder ends the tree after
five idle minutes with no admitted, running or retained operation; the next
admission starts it again.

## Declaration

`installer/declarations/lemonade-image.json` declares the provider
`lemonade-image`, program `lemonaded.exe`, arguments `serve --endpoint
{endpoint}`, transport `oa-native@1`, activation `on_demand`, role
`provider`, the five services above, the modalities `image`,
`transcription` and `speech`, the models `image:user.LocalApprovedKlein`,
`transcription:Whisper-Large-v3-Turbo` and `speech:kokoro-v1`, and the
image extension `lemonade/seed`. The name says image because the provider
began as a picture provider; renaming it is TODO D7.

## Resources

The installed declaration names `card:0`. Before starting Lemonade, the
provider proves the backend and takes a card [lease](https://openabstractions.github.io/reference.html#concept-resource.lease) through the resource kit's
`holder`, using the declared models' pinned admission estimate. The lease is
bound to the engine process, renewed every eight seconds and released when
the tree stops. The `gpufault` Guard handles driver resets and quarantine.
The provider takes no wake lease. See [packaging/README.md](packaging/README.md)
for the lifecycle and remaining limits.

## Rights

Applications never reach the provider's endpoint. The runtime binds the
caller, decides `abstraction.inference/complete` on `server:lemonade-image`
(the wire carries `host:lemonade-image`), checks the content rights the
call needs (`output:image` and `output:speech` for results, `input:image`
for an edit, `input:audio` for transcription), and forwards. The provider
admits only the runtime executable installed beside it. The runtime writes
the provider program's card-hold rules from its declaration through the shared
card-rules writer.

## Tests

From the workspace root:

```text
go test ./openabstractions-flat/abstraction-provider-lemonade/go/...
```

The unit tests cover the command line (`cmd/lemonaded`), admission and the
audio requests (`provider`), the launch, routes, request checks, manifests
and package table (`server`), and staging, activation, retirement and the
private folders (`setup`). Setup's `monitor/internal/setup/lemonade_pins_test.go`
refuses a pin the package table does not know. These tests run on real
hardware only when asked:

| Test | Set |
| --- | --- |
| `server.TestLiveApprovedBinary` | `OA_LEMONADE_TEST_BINARY` |
| `server.TestLiveApprovedBF16` | `OA_LEMONADE_TEST_BINARY`, `OA_LEMONADE_TEST_STATE`, `OA_LEMONADE_BF16_MAIN`, `OA_LEMONADE_BF16_ENCODER`, `OA_LEMONADE_BF16_VAE`; `OA_LEMONADE_TEST_CANCEL=1` adds a cancel |
| `setup.TestLiveStageApprovedVulkanBackend` | `OA_LEMONADE_VULKAN_ARCHIVE` |
| `setup.TestLiveStageReviewedLlamaBackend` | `OA_LLAMA_B10825_ZIP` |

Tests of the launch plan, its Windows environment block and the runtime's
pipe endpoint skip off Windows; the rest run on every platform.

## Platforms

Windows x64 only. The one reviewed package is Lemonade's Windows x64 build
([packaging/windows-amd64.json](packaging/windows-amd64.json)), and Setup's
catalogue pins it for `windows/amd64` alone. The provider builds elsewhere
and refuses every launch there (`server/system_other.go`).

## Status

Development provider for OA 0.4.0, not published.

## Licence

Apache-2.0, as the rest of this repository. Lemonade and the picture models
carry their own licences, listed in
[packaging/windows-amd64.json](packaging/windows-amd64.json) and
`go/server/models.go`. `koko.exe` and `espeak-ng-data` are
GPL-3.0-or-later; OA never ships them. They are fetched from their publisher,
pinned in `installer/third-party.tsv`; no Setup step downloads them or shows
consent text for them yet.
