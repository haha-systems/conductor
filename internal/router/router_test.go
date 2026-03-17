package router

import (
	"context"
	"testing"

	"github.com/haha-systems/conductor/internal/config"
	"github.com/haha-systems/conductor/internal/domain"
	"github.com/haha-systems/conductor/internal/provider"
)

type stubProvider struct {
	name            string
	costPer1kTokens float64
}

func (s *stubProvider) Name() string                      { return s.name }
func (s *stubProvider) Capabilities() []provider.Capability { return nil }
func (s *stubProvider) CostEstimate(promptLen int) (float64, bool) {
	if s.costPer1kTokens <= 0 {
		return 0, false
	}
	return (float64(promptLen) / 4000.0) * s.costPer1kTokens, true
}
func (s *stubProvider) Run(_ context.Context, _ provider.RunContext) (provider.RunHandle, error) {
	return nil, nil
}

func makeProviders(names ...string) map[string]provider.AgentProvider {
	m := make(map[string]provider.AgentProvider, len(names))
	for _, n := range names {
		m[n] = &stubProvider{name: n}
	}
	return m
}

func makePersonas(names ...string) map[string]config.PersonaConfig {
	m := make(map[string]config.PersonaConfig, len(names))
	for _, n := range names {
		m[n] = config.PersonaConfig{Name: n, Dir: "/tmp/personas/" + n}
	}
	return m
}

func makePersonasWithProvider(name, providerName string) map[string]config.PersonaConfig {
	return map[string]config.PersonaConfig{
		name: {Name: name, Provider: providerName, Dir: "/tmp/personas/" + name},
	}
}

func TestRouter_PersonaFrontMatter(t *testing.T) {
	personas := makePersonas("lead-engineer")
	r := NewWithPersonas(makeProviders("claude", "gemini"), nil, nil, personas, "round-robin", "claude")
	task := &domain.Task{Config: &domain.TaskConfig{Persona: "lead-engineer"}}
	result, err := r.Route(task)
	if err != nil {
		t.Fatal(err)
	}
	if result.Persona == nil {
		t.Fatal("expected persona to be set")
	}
	if result.Persona.Name != "lead-engineer" {
		t.Errorf("expected lead-engineer, got %s", result.Persona.Name)
	}
	// No provider override — uses default.
	if result.Providers[0].Name() != "claude" {
		t.Errorf("expected claude (default), got %s", result.Providers[0].Name())
	}
}

func TestRouter_PersonaFrontMatter_WithProviderOverride(t *testing.T) {
	personas := makePersonasWithProvider("lead-engineer", "gemini")
	r := NewWithPersonas(makeProviders("claude", "gemini"), nil, nil, personas, "round-robin", "claude")
	task := &domain.Task{Config: &domain.TaskConfig{Persona: "lead-engineer"}}
	result, err := r.Route(task)
	if err != nil {
		t.Fatal(err)
	}
	if result.Providers[0].Name() != "gemini" {
		t.Errorf("expected gemini (persona override), got %s", result.Providers[0].Name())
	}
}

func TestRouter_PersonaLabelRoute(t *testing.T) {
	personas := makePersonas("lead-engineer")
	personaRoutes := map[string]string{"feature": "lead-engineer"}
	r := NewWithPersonas(makeProviders("claude"), nil, personaRoutes, personas, "round-robin", "claude")
	task := &domain.Task{Labels: []string{"conductor", "feature"}}
	result, err := r.Route(task)
	if err != nil {
		t.Fatal(err)
	}
	if result.Persona == nil || result.Persona.Name != "lead-engineer" {
		t.Errorf("expected lead-engineer persona via label route, got %v", result.Persona)
	}
}

func TestRouter_PersonaUnknown_FallsThrough(t *testing.T) {
	// Unknown persona name in front-matter → falls through to default provider.
	r := NewWithPersonas(makeProviders("claude"), nil, nil, map[string]config.PersonaConfig{}, "round-robin", "claude")
	task := &domain.Task{Config: &domain.TaskConfig{Persona: "nonexistent"}}
	result, err := r.Route(task)
	if err != nil {
		t.Fatal(err)
	}
	if result.Persona != nil {
		t.Error("expected nil persona for unknown persona name")
	}
	if result.Providers[0].Name() != "claude" {
		t.Errorf("expected claude (default), got %s", result.Providers[0].Name())
	}
}

func TestRouter_AgentWinsOverPersona(t *testing.T) {
	// agent: in front-matter wins over persona:.
	personas := makePersonas("lead-engineer")
	r := NewWithPersonas(makeProviders("claude", "codex"), nil, nil, personas, "round-robin", "claude")
	task := &domain.Task{Config: &domain.TaskConfig{Agent: "codex", Persona: "lead-engineer"}}
	result, err := r.Route(task)
	if err != nil {
		t.Fatal(err)
	}
	if result.Persona != nil {
		t.Error("expected no persona when agent: is pinned")
	}
	if result.Providers[0].Name() != "codex" {
		t.Errorf("expected codex, got %s", result.Providers[0].Name())
	}
}

func TestRouter_PersonaRouteBeforeLabelRoute(t *testing.T) {
	// persona_routes match should take precedence over label_routes for the same label.
	personas := makePersonas("lead-engineer")
	personaRoutes := map[string]string{"feature": "lead-engineer"}
	labelRoutes := map[string]string{"feature": "gemini"}
	r := NewWithPersonas(makeProviders("claude", "gemini"), labelRoutes, personaRoutes, personas, "round-robin", "claude")
	task := &domain.Task{Labels: []string{"feature"}}
	result, err := r.Route(task)
	if err != nil {
		t.Fatal(err)
	}
	if result.Persona == nil || result.Persona.Name != "lead-engineer" {
		t.Errorf("expected persona route to win, got persona=%v", result.Persona)
	}
}

func TestRouter_Pinned(t *testing.T) {
	r := New(makeProviders("claude", "codex"), nil, "round-robin", "claude")
	task := &domain.Task{Config: &domain.TaskConfig{Agent: "codex"}}
	result, err := r.Route(task)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Providers) != 1 || result.Providers[0].Name() != "codex" {
		t.Errorf("expected codex, got %v", result.Providers)
	}
}

func TestRouter_Pinned_UnknownProvider(t *testing.T) {
	r := New(makeProviders("claude"), nil, "round-robin", "claude")
	_, err := r.Route(&domain.Task{Config: &domain.TaskConfig{Agent: "nonexistent"}})
	if err == nil {
		t.Error("expected error for unknown provider")
	}
}

func TestRouter_LabelBased(t *testing.T) {
	routes := map[string]string{"big-context": "gemini"}
	r := New(makeProviders("claude", "gemini"), routes, "round-robin", "claude")
	task := &domain.Task{Labels: []string{"conductor", "big-context"}}
	result, err := r.Route(task)
	if err != nil {
		t.Fatal(err)
	}
	if result.Providers[0].Name() != "gemini" {
		t.Errorf("expected gemini, got %s", result.Providers[0].Name())
	}
}

func TestRouter_RoundRobin(t *testing.T) {
	r := New(makeProviders("a", "b"), nil, "round-robin", "a")
	task := &domain.Task{}
	seen := map[string]int{}
	for range 4 {
		result, err := r.Route(task)
		if err != nil {
			t.Fatal(err)
		}
		seen[result.Providers[0].Name()]++
	}
	if seen["a"] != 2 || seen["b"] != 2 {
		t.Errorf("expected even distribution, got %v", seen)
	}
}

func TestRouter_Cheapest(t *testing.T) {
	providers := map[string]provider.AgentProvider{
		"expensive": &stubProvider{name: "expensive", costPer1kTokens: 0.030},
		"cheap":     &stubProvider{name: "cheap", costPer1kTokens: 0.001},
		"mid":       &stubProvider{name: "mid", costPer1kTokens: 0.010},
	}
	r := New(providers, nil, "cheapest", "expensive")
	result, err := r.Route(&domain.Task{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Providers[0].Name() != "cheap" {
		t.Errorf("expected cheap provider, got %s", result.Providers[0].Name())
	}
}

func TestRouter_Cheapest_AllUnknown_FallsBackToRoundRobin(t *testing.T) {
	r := New(makeProviders("a", "b"), nil, "cheapest", "a")
	// stubProvider returns (0, false) when costPer1kTokens == 0
	result, err := r.Route(&domain.Task{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Providers) != 1 {
		t.Errorf("expected 1 provider, got %d", len(result.Providers))
	}
}

func TestRouter_FrontMatterRouting_Cheapest(t *testing.T) {
	providers := map[string]provider.AgentProvider{
		"expensive": &stubProvider{name: "expensive", costPer1kTokens: 0.030},
		"cheap":     &stubProvider{name: "cheap", costPer1kTokens: 0.001},
	}
	r := New(providers, nil, "round-robin", "expensive")
	// Per-task front-matter overrides global strategy.
	task := &domain.Task{Config: &domain.TaskConfig{Routing: "cheapest"}}
	result, err := r.Route(task)
	if err != nil {
		t.Fatal(err)
	}
	if result.Providers[0].Name() != "cheap" {
		t.Errorf("expected cheap, got %s", result.Providers[0].Name())
	}
}

func TestRouter_Race(t *testing.T) {
	r := New(makeProviders("a", "b", "c"), nil, "race 2", "a")
	result, err := r.Route(&domain.Task{})
	if err != nil {
		t.Fatal(err)
	}
	if result.RaceN != 2 {
		t.Errorf("expected RaceN=2, got %d", result.RaceN)
	}
	if len(result.Providers) != 2 {
		t.Errorf("expected 2 providers, got %d", len(result.Providers))
	}
	// Each selected provider should be distinct.
	if result.Providers[0].Name() == result.Providers[1].Name() {
		t.Errorf("race providers should be distinct, got %s and %s",
			result.Providers[0].Name(), result.Providers[1].Name())
	}
}

func TestRouter_Race_FewerProvidersThanN(t *testing.T) {
	r := New(makeProviders("a"), nil, "race 5", "a")
	result, err := r.Route(&domain.Task{})
	if err != nil {
		t.Fatal(err)
	}
	if result.RaceN != 1 {
		t.Errorf("expected RaceN capped at 1 (number of providers), got %d", result.RaceN)
	}
}

func TestRouter_Race_FrontMatter(t *testing.T) {
	r := New(makeProviders("a", "b", "c"), nil, "round-robin", "a")
	task := &domain.Task{Config: &domain.TaskConfig{Routing: "race 3"}}
	result, err := r.Route(task)
	if err != nil {
		t.Fatal(err)
	}
	if result.RaceN != 3 {
		t.Errorf("expected RaceN=3, got %d", result.RaceN)
	}
}

func TestRouter_Default(t *testing.T) {
	r := New(makeProviders("claude"), nil, "unknown-strategy", "claude")
	result, err := r.Route(&domain.Task{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Providers[0].Name() != "claude" {
		t.Errorf("expected claude, got %s", result.Providers[0].Name())
	}
}

func TestRouter_NoProviders(t *testing.T) {
	r := New(map[string]provider.AgentProvider{}, nil, "round-robin", "")
	_, err := r.Route(&domain.Task{})
	if err == nil {
		t.Error("expected error when no providers available")
	}
}

func TestRouter_Persona_FrontMatter(t *testing.T) {
	personas := map[string]config.PersonaConfig{
		"lead-engineer": {Name: "lead-engineer", Provider: "claude"},
	}
	r := NewWithPersonas(makeProviders("claude", "gemini"), nil, nil, personas, "round-robin", "claude")
	task := &domain.Task{Config: &domain.TaskConfig{Persona: "lead-engineer"}}
	result, err := r.Route(task)
	if err != nil {
		t.Fatal(err)
	}
	if result.Providers[0].Name() != "claude" {
		t.Errorf("expected claude, got %s", result.Providers[0].Name())
	}
	if result.Persona == nil || result.Persona.Name != "lead-engineer" {
		t.Errorf("expected persona=lead-engineer, got %v", result.Persona)
	}
}

func TestRouter_Persona_LabelRoute(t *testing.T) {
	personas := map[string]config.PersonaConfig{
		"project-manager": {Name: "project-manager", Provider: "gemini"},
	}
	personaRoutes := map[string]string{"planning": "project-manager"}
	r := NewWithPersonas(makeProviders("claude", "gemini"), nil, personaRoutes, personas, "round-robin", "claude")
	task := &domain.Task{Labels: []string{"conductor", "planning"}}
	result, err := r.Route(task)
	if err != nil {
		t.Fatal(err)
	}
	if result.Providers[0].Name() != "gemini" {
		t.Errorf("expected gemini (persona's provider), got %s", result.Providers[0].Name())
	}
	if result.Persona == nil || result.Persona.Name != "project-manager" {
		t.Errorf("expected persona=project-manager, got %v", result.Persona)
	}
}

func TestRouter_Persona_NoPersonaToml_UsesDefault(t *testing.T) {
	// Persona with no provider override should use defaultName.
	personas := map[string]config.PersonaConfig{
		"lead-engineer": {Name: "lead-engineer"}, // no Provider
	}
	r := NewWithPersonas(makeProviders("claude"), nil, nil, personas, "round-robin", "claude")
	task := &domain.Task{Config: &domain.TaskConfig{Persona: "lead-engineer"}}
	result, err := r.Route(task)
	if err != nil {
		t.Fatal(err)
	}
	if result.Providers[0].Name() != "claude" {
		t.Errorf("expected default claude, got %s", result.Providers[0].Name())
	}
}

func TestRouter_Persona_UnknownFallsThrough(t *testing.T) {
	// Unknown persona in front-matter → falls through to strategy.
	r := NewWithPersonas(makeProviders("claude"), nil, nil, map[string]config.PersonaConfig{}, "round-robin", "claude")
	task := &domain.Task{Config: &domain.TaskConfig{Persona: "nonexistent"}}
	result, err := r.Route(task)
	if err != nil {
		t.Fatal(err)
	}
	if result.Providers[0].Name() != "claude" {
		t.Errorf("expected claude fallthrough, got %s", result.Providers[0].Name())
	}
	if result.Persona != nil {
		t.Errorf("expected nil persona for unknown name, got %v", result.Persona)
	}
}

func TestRouter_Persona_AgentPinWins(t *testing.T) {
	// agent: in front-matter always wins over persona:.
	personas := map[string]config.PersonaConfig{
		"lead-engineer": {Name: "lead-engineer", Provider: "gemini"},
	}
	r := NewWithPersonas(makeProviders("claude", "gemini"), nil, nil, personas, "round-robin", "claude")
	task := &domain.Task{Config: &domain.TaskConfig{Agent: "claude", Persona: "lead-engineer"}}
	result, err := r.Route(task)
	if err != nil {
		t.Fatal(err)
	}
	if result.Providers[0].Name() != "claude" {
		t.Errorf("agent: should win, got %s", result.Providers[0].Name())
	}
	if result.Persona != nil {
		t.Errorf("expected nil persona when agent: is pinned, got %v", result.Persona)
	}
}
