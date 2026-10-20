package server

import (
	"fmt"
	"strings"

	"github.com/openabstractions/abstraction-provider-kit/go/llamacpp"
)

// ErrForbiddenLlamaFlag reports a llama-server option the llama.cpp b10825
// review forbids. It is the provider kit's error: providerhost/llamacpp owns
// the option list (inference CONTRACT.md INF-LC1).
var ErrForbiddenLlamaFlag = llamacpp.ErrForbiddenFlag

// checkLlamaArgs refuses a llama-server argument string that names a
// forbidden option alone or as `--option=value`, and any argument text with
// a quote character: Lemonade's tokenizer splices quoted pieces into one
// word (`--ho"st"` becomes `--host`). Only empty argument text reaches
// Lemonade today; the locked config holds none and requests may carry none.
func checkLlamaArgs(args string) error {
	if strings.ContainsAny(args, `"'`) {
		return fmt.Errorf("%w: quoted argument text", ErrForbiddenLlamaFlag)
	}
	for _, word := range strings.Fields(args) {
		if err := llamacpp.CheckFlag(word); err != nil {
			return err
		}
	}
	return nil
}

// checkBackendArgs refuses a configuration whose backend sections carry any
// argument string. Review condition 2 keeps llamacpp_args empty in
// config.json, and the Lemonade review's condition 4 keeps every `*_args`
// empty; a forbidden llama.cpp option is named as one.
func checkBackendArgs(cfg map[string]any) error {
	for section, value := range cfg {
		values, ok := value.(map[string]any)
		if !ok {
			continue
		}
		for key, arg := range values {
			if key != "args" && !strings.HasSuffix(key, "_args") {
				continue
			}
			text, ok := arg.(string)
			if !ok {
				return fmt.Errorf("%w: %s.%s is not a string", ErrLockedConfig, section, key)
			}
			if err := checkLlamaArgs(text); err != nil {
				return fmt.Errorf("%w: %s.%s: %w", ErrLockedConfig, section, key, err)
			}
			if text != "" {
				return fmt.Errorf("%w: %s.%s carries backend arguments", ErrLockedConfig, section, key)
			}
		}
	}
	return nil
}
