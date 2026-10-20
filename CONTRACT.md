# provider-lemonade contract

The contracts this provider serves are
[`abstraction.inference/image@1`](https://github.com/openabstractions/abstraction-inference/blob/main/CONTRACT.md),
`abstraction.inference/transcription@1` and `abstraction.inference/speech@1`,
whose words bind every call, with `abstraction.storage/content-reader@1` and
`abstraction.storage/content-writer@1` for the runtime alone. The rules below
say what `lemonaded` does around them: which Lemonade build it runs, what it
proves before each launch, which requests reach Lemonade, and what it
refuses. [README.md](README.md) is the walkthrough;
[packaging/README.md](packaging/README.md) carries the installer feature,
the package records and each security review condition in full.

Binds: [inference.thrift](https://github.com/openabstractions/abstraction-inference/blob/main/inference.thrift)

## Reading this page

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD",
"SHOULD NOT", "RECOMMENDED", "NOT RECOMMENDED", "MAY", and "OPTIONAL" in this
document are to be interpreted as described in BCP 14 [RFC2119] [RFC8174]
when, and only when, they appear in all capitals, as shown here.

The rule ids below, such as `LEMONADE-A1`, are what tests and refusals
cite. A retired id is never reused; [HISTORY.md](HISTORY.md) keeps it with
the release it left, beside this page's design records.

| Letter | Meaning |
| --- | --- |
| A | admission |
| P | the package and its files |
| L | the launch |
| I | image requests |
| C | the chat route |
| S | transcription and speech |
| H | holding the card |
| R | rights |
| E | outcomes |

| Prose | Wire (until its own release) |
| --- | --- |
| resource `server:lemonade-image` | resource `host:lemonade-image` (`research/vocabulary/RENAME-PLAN.md` §4) |
| modality `image`, `transcription`, `speech` | `profile:image`, `profile:transcription`, `profile:speech` |

## Admission

**[LEMONADE-A1] One profile table decides every model.** The service MUST
serve a model only for the modality its one table names
(`server.ProfileModels`, router CONTRACT.md ROUTE-D9): the selection's
picture models for `image`, `Whisper-Large-v3-Turbo` for `transcription`,
`kokoro-v1` for `speech`. The router host, the admission hook and the image
body check read that table. A request for a model the table does not name
for its modality is refused `unavailable` with reason `model:undeclared`
before Lemonade starts (inference CONTRACT.md INF-A5).

**[LEMONADE-A2] Admission starts the engine in order.** The admission hook
MUST check, in this order: the model (LEMONADE-A1); for an audio model, that
its files are staged (`engine:audio:engine-files`,
`engine:audio:model-files`); the backend folders (LEMONADE-P2); then it
starts or reuses the Lemonade tree (LEMONADE-L1); then, for an audio model,
that its route is open for this launch (LEMONADE-S1). The first failure is
the refusal, `unavailable` with its reason word (LEMONADE-E1); the full
error goes to the provider's log alone.

## The package and its files

**[LEMONADE-P1] The package is a pinned Lemonade version.** The service MUST
run only a Lemonade version `server.Packages` lists: its archive and
`lemond.exe` SHA-256, its package manifest, its paired sd.cpp and llama.cpp
manifests, its backend archive SHA-256, its model revision and its locked
configuration. The table only grows. The selection file names the active
entry; each launch reads it immediately before it proves and holds the
files. A run in progress finishes on the version it proved. Today the
table holds Lemonade 2026.39.1 (`lemonade-embeddable-v2026.39.1-windows-amd64`).

**[LEMONADE-P2] Every engine file is proved and held.** Before each launch
the service MUST prove the package folder and every backend folder under
`engine-cache/<version>/bin` against their manifests through the provider
kit's engine-file proof (inference CONTRACT.md INF-F1..INF-F5), and hold the
files until the Lemonade tree exits. A changed, missing or extra file or
folder, a link, a folder without the read-only ACL, or a backend of another
build is refused: `engine:package-unavailable` for the package,
`engine:backend-unavailable` for a backend. The sd.cpp backend holds 21
files; the llama.cpp b10825 backend holds the 24 files its review keeps, and
`ggml-rpc-server.exe`, `ggml-rpc.dll` and the command-line tools are absent.

**[LEMONADE-P3] Setup moves the selection through fixed verbs.** `package
check`, `package activate`, `package retire`, `model add`, `configure` and
`engine unlock` are Setup's, and the provider calls none of them itself.
`package activate` writes the selection last under the setup lock. `package
retire` refuses the selected version and exits 3 while a launch holds it. A
refusal of a package verb exits 2; any other failure exits 1. Model files
move into the provider's state by rename from a download on the models
folder's volume, and OA never copies a model for the engine.

**[LEMONADE-P4] Weights are named formats in named places.** A model the
service declares MUST be safetensors or GGUF, in its models folder and its
model registry (`engine:weight-format` otherwise). A declared model that
resolves to a Lemonade collection refuses the configuration and each launch
with `engine:collection-model`.

## The launch

**[LEMONADE-L1] The service starts its own Lemonade tree.** The service
MUST start `lemond.exe` from the proven package folder as its working
directory, inside a contained process tree, with the private folders,
`--port <private port>`, `--host 127.0.0.1` and `--no-broadcast` on a
checked argument list, and stop it by that tree. It never uses a Lemonade
server another program started. Lemonade starts `sd-server`,
`llama-server`, `whisper-server` and `koko` from the proven backend folders
inside the same tree.

**[LEMONADE-L2] The environment is built from nothing.** The tree MUST
start with `SystemRoot`, `windir`, `SystemDrive`, `PATH` of the Windows
system folders, `TEMP` and `TMP` at `engine-temp`, and two random 256-bit
keys per launch as `LEMONADE_API_KEY` and `LEMONADE_ADMIN_API_KEY`. The
forbidden names (`GGML_*`, `MTMD_*`, `LLAMA_*`, `HF_*`, other `LEMONADE_*`,
`WHISPER_*`, `OVERRIDE_VITISAI_*`, `KMP_*`, `PIPER_*`, `ESPEAK*`, `ORT_*`,
`SSL_CERT_*` and every proxy name) are refused even if the allowlist names
one (`engine:launch-plan`). Every provider request carries one of the keys.

**[LEMONADE-L3] The configuration is locked.** The version's
`engine-config/<version>/config.json` MUST equal the locked bytes of that
version's configuration: loopback host, no broadcast, offline, no
executable fetching, every backend `*_bin` built in, every `*_args` empty,
and `whispercpp.backend` `vulkan`. Any other file in that folder except
`user_models.json`, and any entry of the cache folder other than `bin`,
refuses the launch (`engine:config-mismatch`).

**[LEMONADE-L4] The tree holds only loopback sockets.** After readiness and
around each request, every socket of the tree MUST be on 127.0.0.1 or `::1`
and the tree MUST hold its listener on `127.0.0.1:<port>` (abstraction-identity
ID-T7, inference CONTRACT.md INF-EL2); otherwise the tree is stopped and the
request refused `engine:listener-not-loopback`.

**[LEMONADE-L5] The tree stops when idle.** The resource holder MUST stop
the tree after five idle minutes when no declared model has an admitted,
running or retained operation. The next admission starts it again. This
lifecycle applies to every modality the provider serves.

## Image requests

**[LEMONADE-I1] Callers reach the image routes alone.** The service MUST
forward an image call only to `POST /api/v1/images/generations` and `POST
/api/v1/images/edits`. `/internal/*`, `/mcp`, pull, install,
`/api/v1/load` and the Ollama routes stay closed.

**[LEMONADE-I2] The provider forwards its own encoding of checked fields.**
An image request MUST be read once it holds the provider's request slot,
bounded by two input images of `MaxImageBytes` (default 20 MiB) and the
text fields, and checked through `providerhost/sdcpp` (inference
CONTRACT.md INF-SD1..INF-SD3). Only `model`, `prompt`, `n`, `size`,
`response_format`, the `seed` the declared extension `lemonade/seed` writes,
and the edit files `image` and `mask` are forwarded, each once; a mask is
converted to the luminance form `sd-server` reads. `<sd_cpp_extra_args` in
any text value (`prompt:engine-directive`), a size beyond 64..4096 per side
or 2048 × 2048 pixels and an input image whose header names such a size
(`size:engine-bound`), and a transfer-encoded or repeated part, a file other
than PNG, JPEG or WebP, or a `model` that is absent or names no declared
picture model (`request:engine-form`) are refused before Lemonade sees the
request, with the reason inference gives each.

## The chat route

**[LEMONADE-C1] Chat passes the llama.cpp b10825 conditions.** The service
MUST forward a request to `POST /api/v1/chat/completions` only while the
five conditions of the llama.cpp b10825 review that apply to Lemonade hold,
and otherwise refuse it with `engine:chat-route:<condition>`: `engine-files`
(the b10825 folder is proven and held for this launch), `load-options` (no
load option in the request or the locked configuration), `environment`,
`media` (an image is a base64 PNG or JPEG of at most 16 MP, which the
provider re-encodes as its own PNG; audio is WAV, MP3 or FLAC; no URL,
video or WebP) and `llama-flags` (no forbidden llama.cpp option in any
argument path the provider controls). The provider serves no `chat`
modality today: the route opens to OA callers once Setup offers a reviewed
chat model with a pinned SHA-256 and the provider declares it.

## Transcription and speech

**[LEMONADE-S1] An audio route opens only on staged, proven files.** The
service MUST open `POST /api/v1/audio/transcriptions` and `POST
/api/v1/audio/speech` only while the launch holds the package folder, the
route's backend and model folders, the explicit backend and a clean
environment, and otherwise refuse with `engine:audio:<condition>`
(`package`, `engine-files`, `model-files`, `backend`, `arguments`,
`environment`, `backoff`). whisper.cpp v1.8.4 Vulkan and Kokoros b17 are
staged by `configure --activate-reviewed-package --speech-only`; the speech
models are admitted by exact path, never by extension.

**[LEMONADE-S2] Audio requests carry the provider's own bytes.** A
transcription MUST be sent as `audio.wav`, the provider's own WAV of the
caller's 16-bit PCM WAV of at most 16 MiB, with `language` from whisper's
table and no other field; its answer is read as JSON with a 4 MiB cap. A
speech request MUST carry an English voice (`af_*`, `am_*`, `bf_*`,
`bm_*`), NFC text without control characters, `response_format` `pcm`,
`speed` 0.5..2.0 and `stream` false; the PCM answer, at most 64 MiB, gets
the provider's own WAV header.

**[LEMONADE-S3] A failed speech request is never resubmitted.** A speech
request whose connection drops or whose engine fails MUST NOT be sent again.
The provider logs the input's SHA-256 alone and refuses speech, and speech
only, for a backoff of 5 seconds doubling to 5 minutes. It stops the tree,
so `koko` starts afresh, only when no request is in flight and no other
model has work in the provider.

## Holding the card

The installed declaration names `card:0`. The provider uses abstraction-resource's
`holder` for a process-bound card lease and its `gpufault` Guard for driver
resets and quarantine (resource CONTRACT.md RES-L1..RES-L4, RES-G1..RES-G7).
Launch order is backend proof, lease acquisition for the pinned admission
estimate, then engine start and readiness. The holder renews the lease every
eight seconds and releases it when the tree stops. The provider takes no
wake lease. [packaging/README.md](packaging/README.md) records the lifecycle
and remaining limits.

## Rights

**[LEMONADE-R1] The runtime is the only caller.** The service MUST admit
only the runtime executable installed beside `lemonaded`
(`openabstractions.exe` in the same folder). Applications reach the
provider through the runtime, which binds the caller, decides
`abstraction.inference/complete` on `server:lemonade-image` and the content
rights the call needs (inference CONTRACT.md INF-S7, INF-V4), and forwards.
The runtime writes the provider program's card-hold rules from its declaration
through the shared card-rules writer.

## Outcomes

**[LEMONADE-E1] Admission refusals are typed.** Every refusal below is
`unavailable` with its reason word; no reason carries engine text.

| Reason | Meaning |
| --- | --- |
| `model:undeclared` | The model serves no declared modality, or another one (LEMONADE-A1). |
| `engine:package-unavailable` | The package folder is not the pinned version's (LEMONADE-P1, P2). |
| `engine:backend-unavailable` | A backend folder is not its pinned manifest (LEMONADE-P2). |
| `engine:config-mismatch` | The configuration folder differs from the locked bytes (LEMONADE-L3). |
| `engine:weight-format` | A declared model is not safetensors or GGUF in its place (LEMONADE-P4). |
| `engine:collection-model` | A declared model resolves to a Lemonade collection (LEMONADE-P4). |
| `engine:launch-plan` | An argument or environment name is outside the checked lists (LEMONADE-L1, L2). |
| `engine:listener-not-loopback` | The tree held a socket beyond loopback or no listener on its port (LEMONADE-L4). |
| `engine:audio:<condition>` | An audio route is closed for this launch (LEMONADE-S1, S3). |
| `engine:chat-route:<condition>` | A chat request fails a llama.cpp review condition (LEMONADE-C1). |
| `engine:unavailable` | Lemonade would not start or was not ready. |

An image request the route check refuses carries `prompt:engine-directive`,
`size:engine-bound` or `request:engine-form` (LEMONADE-I2).

## Bounds

The service starts no process it cannot stop by its tree, and forwards no
byte it did not check and encode itself.

## Divergences

None declared for this release.

## Not built

`sd-server`, `whisper-server` and `koko` have no authentication of their
own: a local process that learns a loopback port can submit work to them
directly, past the provider's checks (packaging/README.md, "Stated
exposure"). The provider's keys protect Lemonade's routes alone. The
declaration's name says image although the provider also serves
transcription and speech; renaming it is TODO D7. Modelhost and this
provider each pin the same sd-server and llama-server builds (TODO D7).
