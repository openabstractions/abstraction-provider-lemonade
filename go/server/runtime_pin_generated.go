// Code generated from vc_runtime_asset.json by installer/modelhost_engine.py; DO NOT EDIT.

package server

const llamaRuntimeArchive = "vc-runtime-14.51.36231-win-x64.zip"

func llamaRuntimeFiles() []ManifestFile {
	return []ManifestFile{
		{Name: "msvcp140.dll", Size: 643512, SHA256: "7c26614e1d733892c2deac7e245ce115504b1d80592dd0a01b08e3e5a55f89ca"},
		{Name: "vcruntime140.dll", Size: 178616, SHA256: "d1f4225df2cd877dbf130d5668a021dce3f94118455ff5ec952061c30afc9ce7"},
		{Name: "vcruntime140_1.dll", Size: 50112, SHA256: "a7146c08f89fe5b04541ab507cdb59ff7b44534d4ba3c668a426c6450a03434e"},
	}
}
