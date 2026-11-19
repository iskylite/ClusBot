package work

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

type BenchmarkRevisionProposalInput struct {
	ExpectedRevision int    `json:"expected_revision"`
	Text             string `json:"text"`
}

type BenchmarkRevisionProposal struct {
	Method           string                  `json:"method"`
	State            string                  `json:"state"`
	ExpectedRevision int                     `json:"expected_revision"`
	Interpretation   BenchmarkRevisionIntent `json:"interpretation"`
	Preview          BenchmarkRevision       `json:"preview"`
}

type BenchmarkRevisionIntent struct {
	Action       string `json:"action"`
	CaseKey      string `json:"case_key"`
	Parameter    string `json:"parameter,omitempty"`
	Value        string `json:"value,omitempty"`
	ChangeActual bool   `json:"change_expected_actual_config,omitempty"`
}

const revisionInstructionHelp = "请用准确的测项 ID，一次只改一处，例如「移除测试项 single-node」或「将测试项 single-node 的参数 gpu_count 改为 32」"

var (
	removeBenchmarkCaseText = regexp.MustCompile(`^(?:请)?移除测试项[ \t]+([a-zA-Z0-9_.-]{1,128})[。]?$`)
	setBenchmarkParamText   = regexp.MustCompile(`^(?:请)?将测试项[ \t]+([a-zA-Z0-9_.-]{1,128})[ \t]+的参数[ \t]+([a-zA-Z0-9_.-]{1,128})[ \t]+改为[ \t]+([a-zA-Z0-9_.+%-]{1,256})([ \t]+并同步实测要求)?[。]?$`)
)

func parseBenchmarkRevisionText(text string) (BenchmarkRevisionIntent, error) {
	text = strings.TrimSpace(text)
	if len(text) > 1024 {
		return BenchmarkRevisionIntent{}, ErrBenchmarkInput
	}
	if m := removeBenchmarkCaseText.FindStringSubmatch(text); m != nil {
		return BenchmarkRevisionIntent{Action: "remove_case", CaseKey: m[1]}, nil
	}
	if m := setBenchmarkParamText.FindStringSubmatch(text); m != nil {
		return BenchmarkRevisionIntent{Action: "set_parameter", CaseKey: m[1], Parameter: m[2], Value: m[3], ChangeActual: m[4] != ""}, nil
	}
	return BenchmarkRevisionIntent{}, fmt.Errorf("%w：%s", ErrBenchmarkInput, revisionInstructionHelp)
}

// ProposeBenchmarkRevision translates a deliberately bounded instruction
// into the existing revision validator. It never applies or submits changes.
// The preview is ephemeral: apply must still recheck the expected revision,
// authorization and whether the job has already crossed the dispatch boundary.
func (s *Store) ProposeBenchmarkRevision(ctx context.Context, actor, task string, input BenchmarkRevisionProposalInput, provider BenchmarkProvider) (BenchmarkRevisionProposal, error) {
	current, err := s.benchmarkRevisionProposalBase(ctx, actor, task, input.ExpectedRevision)
	if err != nil {
		return BenchmarkRevisionProposal{}, err
	}
	intent, err := parseBenchmarkRevisionText(input.Text)
	if err != nil {
		return BenchmarkRevisionProposal{}, err
	}
	return s.previewBenchmarkRevisionIntent(ctx, actor, task, current, intent, provider, "explicit_shortcut")
}

func (s *Store) benchmarkRevisionProposalBase(ctx context.Context, actor, task string, expected int) (BenchmarkRevision, error) {
	if expected < 1 {
		return BenchmarkRevision{}, ErrBenchmarkInput
	}
	current, err := s.ReadBenchmarkRevision(ctx, actor, task, 0)
	if err != nil {
		return BenchmarkRevision{}, err
	}
	if expected != current.Revision {
		return BenchmarkRevision{}, fmt.Errorf("%w：当前测试方案修订为 %d", ErrTaskRevision, current.Revision)
	}
	return current, nil
}

func (s *Store) previewBenchmarkRevisionIntent(ctx context.Context, actor, task string, current BenchmarkRevision, intent BenchmarkRevisionIntent, provider BenchmarkProvider, method string) (BenchmarkRevisionProposal, error) {
	cases := slices.Clone(current.Profile.Cases)
	index := slices.IndexFunc(cases, func(c BenchmarkCase) bool { return c.Key == intent.CaseKey })
	if index < 0 {
		return BenchmarkRevisionProposal{}, fmt.Errorf("%w：当前方案没有这个测项 ID", ErrBenchmarkInput)
	}
	if intent.Action == "remove_case" {
		cases = slices.Delete(cases, index, index+1)
	} else {
		c := &cases[index]
		if _, ok := c.Parameters[intent.Parameter]; !ok {
			return BenchmarkRevisionProposal{}, fmt.Errorf("%w：测项没有这个参数，不能自动新增", ErrBenchmarkInput)
		}
		expected, hasExpected := c.ActualConfig[intent.Parameter]
		if intent.ChangeActual && !hasExpected {
			return BenchmarkRevisionProposal{}, fmt.Errorf("%w：测项没有同名实测要求，不能自动新增", ErrBenchmarkInput)
		}
		if !intent.ChangeActual && hasExpected && expected != intent.Value {
			return BenchmarkRevisionProposal{}, fmt.Errorf("%w：该参数也用于核对实测配置，请明确是否「并同步实测要求」", ErrBenchmarkInput)
		}
		c.Parameters = maps.Clone(c.Parameters)
		c.Parameters[intent.Parameter] = intent.Value
		if intent.ChangeActual {
			c.ActualConfig = maps.Clone(c.ActualConfig)
			c.ActualConfig[intent.Parameter] = intent.Value
		}
	}
	preview, err := s.ReviseBenchmark(ctx, actor, task, BenchmarkRevisionInput{
		ExpectedRevision: current.Revision, Cases: cases,
	}, provider, false)
	if err != nil {
		return BenchmarkRevisionProposal{}, err
	}
	return BenchmarkRevisionProposal{Method: method, State: "preview_only", ExpectedRevision: current.Revision, Interpretation: intent, Preview: preview}, nil
}
