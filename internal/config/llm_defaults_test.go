package config

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/webauto"
)

// TestApplyAutomationLLMDefaults_BuiltIns: with nothing configured, every kind
// gets the built-in seats — internal loop (do/step legs) → Sonnet, judge →
// Opus 4.8.
func TestApplyAutomationLLMDefaults_BuiltIns(t *testing.T) {
	var a AutomationConfig
	applyAutomationLLMDefaults(&a)
	for name, k := range map[string]*AutomationKindConfig{
		"web": &a.Web, "task": &a.Task, "agent": &a.Agent, "humanoid": &a.Humanoid, "code": &a.Code,
	} {
		eff := webauto.EffectiveStepDef(k.Step, k.Loop)
		if eff == nil || eff.Model != webauto.DefaultStepModel {
			t.Errorf("%s: step model = %+v, want %s", name, eff, webauto.DefaultStepModel)
		}
		if k.Loop == nil || k.Loop.While == nil || k.Loop.While.Model != webauto.DefaultJudgeModel {
			t.Errorf("%s: judge model missing, want %s", name, webauto.DefaultJudgeModel)
		}
	}
}

// TestApplyAutomationLLMDefaults_GlobalLoopTier: automation.loop overrides the
// built-ins for kinds that don't declare their own.
func TestApplyAutomationLLMDefaults_GlobalLoopTier(t *testing.T) {
	a := AutomationConfig{
		Loop: &webauto.LoopConfig{
			Do:    &webauto.StepConfig{Model: "claude-haiku-4-5-20251001", Effort: "low"},
			While: &webauto.WhileConfig{Model: "claude-fable-5"},
		},
	}
	applyAutomationLLMDefaults(&a)
	if a.Web.Step.Model != "claude-haiku-4-5-20251001" || a.Web.Step.Effort != "low" {
		t.Errorf("web step seat = %+v", a.Web.Step)
	}
	if a.Task.Loop.While.Model != "claude-fable-5" {
		t.Errorf("task judge seat = %+v", a.Task.Loop.While)
	}
}

// TestApplyAutomationLLMDefaults_KindWins: a kind's own declarations survive
// the folding — including a loop.do block, which must not be shadowed and
// must not lose a sibling configured step.
func TestApplyAutomationLLMDefaults_KindWins(t *testing.T) {
	a := AutomationConfig{
		Web: AutomationKindConfig{
			Loop: &webauto.LoopConfig{
				Do:    &webauto.StepConfig{Model: "claude-opus-4-8", Interval: 10},
				While: &webauto.WhileConfig{Model: "claude-sonnet-5", Effort: "high"},
			},
		},
		Task: AutomationKindConfig{
			Step: &webauto.StepConfig{Interval: 20},
		},
	}
	applyAutomationLLMDefaults(&a)
	if a.Web.Loop.Do.Model != "claude-opus-4-8" || a.Web.Loop.Do.Interval != 10 {
		t.Errorf("web loop.do clobbered: %+v", a.Web.Loop.Do)
	}
	if a.Web.Loop.While.Model != "claude-sonnet-5" || a.Web.Loop.While.Effort != "high" {
		t.Errorf("web judge clobbered: %+v", a.Web.Loop.While)
	}
	// Task kind: its configured step keeps non-model fields and gains the
	// model default in place.
	if a.Task.Step.Interval != 20 || a.Task.Step.Model != webauto.DefaultStepModel {
		t.Errorf("task step = %+v", a.Task.Step)
	}
}
