package server

// AdmissionEstimate is the card memory one launch asks its lease for: the
// pinned files of the largest declared picture model, plus those of the
// Whisper and Kokoro models when their profiles are served. It is an
// admission estimate read from the pins, never a measurement. ok is false
// when a declared picture model has no pins in ImageModels, as an advanced
// launch's own model may; the caller then names the amount itself.
func (s *Server) AdmissionEstimate() (bytes int64, ok bool) {
	var picture int64
	for _, id := range s.cfg.Models {
		m, found := FindImageModel(id)
		if !found {
			return 0, false
		}
		picture = max(picture, m.Bytes())
	}
	bytes = picture
	if s.cfg.Transcription {
		bytes += s.cfg.WhisperModel.bytes()
	}
	if s.cfg.Speech {
		bytes += s.cfg.KokoroModel.bytes()
	}
	return bytes, bytes > 0
}

// bytes is the total size of the model's pinned files.
func (m SpeechModel) bytes() int64 {
	var total int64
	for _, f := range m.Files {
		total += f.Size
	}
	return total
}
