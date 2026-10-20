// Package provider composes Lemonade's owned server with the shared native
// engine provider host. It serves image@1 and, when the server enables them,
// transcription@1 through whisper.cpp and speech@1 through Kokoros. Image
// request mapping and publication stay in inference; the audio requests are
// built by the provider kit's whisper.cpp and Kokoros policy packages.
package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	inference "github.com/openabstractions/abstraction-inference/go"
	"github.com/openabstractions/abstraction-provider-kit/go/providerhost"
	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
	"github.com/openabstractions/abstraction-resource/go/gpufault"
	"github.com/openabstractions/abstraction-resource/go/holder"
	router "github.com/openabstractions/abstraction-router/go"
)

type Config struct {
	Name            string
	Endpoint        string
	RuntimePrograms []string
	StoreRoot       string
	RecordPath      string
	Server          server.Config
	// OnError receives provider faults, including a failed audit append.
	OnError func(error)

	// Leases is the resource service's leases@1, through which the tree
	// holds its card. It is required: every launch takes a lease of
	// Resource first and gives it back when the tree stops (card.go).
	Leases holder.Leases
	// Resource is the card the engines load into; empty reads CardResource.
	Resource string
	// Amount is the lease's admission estimate in bytes; zero reads the
	// server's AdmissionEstimate from the declared models' pins.
	Amount int64
	// Idle, Renew and Wait are the holder's idle unload, renewal interval
	// and lease wait; zero reads the holder's defaults.
	Idle, Renew, Wait time.Duration
	// GPU is the provider's GPU fault handling: it is asked before every
	// launch and stops the tree on a driver reset (resource CONTRACT.md
	// RES-G1..RES-G7). Nil handles none.
	GPU *gpufault.Guard
}

type Provider struct {
	Host   *providerhost.Host
	Server *server.Server
	card   *card
}

func New(cfg Config) (*Provider, error) {
	s, err := server.New(cfg.Server)
	if err != nil {
		return nil, err
	}
	amount := cfg.Amount
	if amount <= 0 {
		estimate, ok := s.AdmissionEstimate()
		if !ok {
			return nil, fmt.Errorf("%w: a declared model has no pinned size; name the card lease's amount", server.ErrConfiguration)
		}
		amount = estimate
	}
	held, err := newCard(s, cardOptions{Leases: cfg.Leases, Resource: cfg.Resource, Amount: amount,
		Idle: cfg.Idle, Renew: cfg.Renew, Wait: cfg.Wait, GPU: cfg.GPU, OnError: cfg.OnError})
	if err != nil {
		return nil, err
	}
	name := cfg.Name
	if name == "" {
		name = "lemonade"
	}
	host, declared := servedHost(name, s)
	engine := providerhost.EngineConfig{Endpoint: cfg.Endpoint, RuntimePrograms: cfg.RuntimePrograms, HTTP: s.HTTP(),
		StoreRoot: cfg.StoreRoot, RecordPath: cfg.RecordPath, MaxImageBytes: cfg.Server.MaxImageBytes, ImageFields: server.ImageFields,
		OnError: cfg.OnError, Host: host, Profiles: host.HostProfiles()}
	if cfg.Server.Transcription {
		engine.TranscriptionEngine = transcriber{server: s, client: s.HTTP(), onError: cfg.OnError}
	}
	if cfg.Server.Speech {
		engine.SpeechEngine = speaker{server: s, client: s.HTTP(), onError: cfg.OnError}
	}
	engine.Admit = admitter(s, host, held.ensure, cfg.OnError)
	native, err := providerhost.NewEngine(engine)
	if err != nil {
		return nil, err
	}
	// The idle stop and the speech crash handling see every admitted or
	// running operation of the provider, image work included.
	s.BindActivity(func(except string) bool {
		for _, model := range declared {
			if model != except && native.Provider.ActiveForModel(model) {
				return true
			}
		}
		return false
	})
	// The lease's idle unload and its yield see the same work.
	held.bindActive(func() bool {
		for _, model := range declared {
			if native.Provider.ActiveForModel(model) {
				return true
			}
		}
		return false
	})
	return &Provider{Host: native, Server: s, card: held}, nil
}

// servedHost is the router host for the owned Lemonade s and every model it
// serves, in profile order. Each model serves only its own profile, read from
// the server's one table (server.ProfileModels, ROUTE-D9). The image profile
// declares server.ImageExtensions, which server.ImageFields carries.
func servedHost(name string, s *server.Server) (*router.Host, []string) {
	profiles, byProfile := s.ProfileModels()
	var declared []string
	for _, profile := range profiles {
		declared = append(declared, byProfile[profile]...)
	}
	host := router.OpenAICompatible(name, s.Base(), "/api/v1", declared)
	host.Profiles = profiles
	host.ScopeModels(byProfile)
	extensions := make(map[string]bool, len(server.ImageExtensions))
	for _, key := range server.ImageExtensions {
		extensions[key] = true
	}
	host.SupportedExtensions = map[string]map[string]bool{router.ProfileImage: extensions}
	return host, declared
}

// admitter is the provider's admission hook (INF-A5). A model the server's
// profile table does not name for the request's profile reads
// model:undeclared before Lemonade starts, the second line behind the
// router's own refusal (ROUTE-D9); the image body check is the third.
// ensure runs the tree under its card lease (card.ensure): the backend
// proof, the lease, then the launch.
func admitter(s *server.Server, host *router.Host, ensure func(context.Context) error, onError func(error)) inference.Admitter {
	return func(ctx context.Context, selected *router.Host, model string) (string, bool) {
		audio := s.AudioModel(model)
		if selected != host || !s.ServesModel(inference.AdmissionProfile(ctx), model) {
			return "model:undeclared", false
		}
		// An unstaged speech profile is refused before Lemonade starts.
		if audio {
			if err := s.AudioStaged(ctx, model); err != nil {
				report(onError, err)
				return server.RefusalReason(err), false
			}
		} else if err := s.ImageStaged(ctx, model); err != nil {
			report(onError, err)
			return server.RefusalReason(err), false
		}
		// The typed reason goes to the caller; the full error, which names
		// the file, socket or lease, goes to the provider's own log.
		if err := ensure(ctx); err != nil {
			report(onError, err)
			return loadReason(err), false
		}
		if audio {
			if err := s.CheckAudio(model); err != nil {
				report(onError, err)
				return server.RefusalReason(err), false
			}
		}
		return "", true
	}
}

func report(onError func(error), err error) {
	if onError != nil && err != nil {
		onError(err)
	}
}

// Serve serves until ctx ends, keeping the card lease renewed beside it, and
// then stops the tree and gives the lease back.
func (p *Provider) Serve(ctx context.Context) error {
	held, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); p.card.run(held) }()
	serveErr := p.Host.Serve(ctx)
	stop()
	<-done
	p.card.close()
	return errors.Join(serveErr, p.Server.Close())
}

func (p *Provider) Close() error {
	p.card.close()
	return errors.Join(p.Host.Close(), p.Server.Close())
}
