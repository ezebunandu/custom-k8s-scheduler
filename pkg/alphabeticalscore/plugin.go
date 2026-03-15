package alphabeticalscore

import (
	"context"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const Name = "AlphabeticalScore"
const DefaultLabelKey = "topology.kubernetes.io/zone"
const PodLabelOverrideKey = "scheduler.io/rank-by-label"

// AlphabeticalScore scores nodes by the alphabetical order of a label value.
// Earlier alphabetically = higher score.
type AlphabeticalScore struct {
	handle   framework.Handle
	labelKey string
}

var _ framework.ScorePlugin = &AlphabeticalScore{}

func (pl *AlphabeticalScore) Name() string { return Name }

// computeScore converts a label value to a numeric score.
// Labels starting with 'a' score highest (100); each subsequent letter subtracts 4.
func computeScore(labelVal string) int64 {
	if len(labelVal) == 0 {
		return framework.MinNodeScore
	}
	first := strings.ToLower(labelVal)[0]
	if first < 'a' || first > 'z' {
		return framework.MinNodeScore
	}
	score := framework.MaxNodeScore - int64(first-'a')*4
	if score < framework.MinNodeScore {
		return framework.MinNodeScore
	}
	return score
}

// Score assigns a score to a node based on the alphabetical position of its label value.
func (pl *AlphabeticalScore) Score(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) (int64, *framework.Status) {
	nodeInfo, err := pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil || nodeInfo == nil || nodeInfo.Node() == nil {
		return framework.MinNodeScore, nil
	}

	labelKey := pl.labelKey
	if p != nil && p.Labels != nil {
		if override, ok := p.Labels[PodLabelOverrideKey]; ok && override != "" {
			labelKey = override
		}
	}

	labelVal, ok := nodeInfo.Node().Labels[labelKey]
	if !ok || labelVal == "" {
		return framework.MinNodeScore, nil
	}

	return computeScore(labelVal), nil
}

func (pl *AlphabeticalScore) ScoreExtensions() framework.ScoreExtensions { return nil }

// New creates a new AlphabeticalScore plugin instance.
func New(_ context.Context, _ runtime.Object, h framework.Handle) (framework.Plugin, error) {
	return &AlphabeticalScore{handle: h, labelKey: DefaultLabelKey}, nil
}
