// Code generated from vc_runtime_asset.json by installer/modelhost_engine.py; DO NOT EDIT.

package setup

import "github.com/openabstractions/abstraction-provider-lemonade/go/server"

const llamaRuntimeVersion = "14.51.36231"
const llamaRuntimeURL = "https://github.com/openabstractions/redist/releases/download/v0.8.1/vc-runtime-14.51.36231-win-x64.zip"

var llamaRuntimeInput = backendRuntime{
	Name:   "vc-runtime-14.51.36231-win-x64.zip",
	Size:   872702,
	SHA256: "b249488d379d1cbac2570080341deec526f1e486ba7a0dbb142cd219e50d87c1",
	Prefix: "Microsoft.VC145.CRT/",
	Files: []server.ManifestFile{
		{Name: "msvcp140.dll", Size: 643512, SHA256: "7c26614e1d733892c2deac7e245ce115504b1d80592dd0a01b08e3e5a55f89ca"},
		{Name: "vcruntime140.dll", Size: 178616, SHA256: "d1f4225df2cd877dbf130d5668a021dce3f94118455ff5ec952061c30afc9ce7"},
		{Name: "vcruntime140_1.dll", Size: 50112, SHA256: "a7146c08f89fe5b04541ab507cdb59ff7b44534d4ba3c668a426c6450a03434e"},
	},
}
