package pipeline

import (
	"fmt"
	"sort"
	"strings"

	"github.com/luiul/orchard/internal/model"
	"github.com/luiul/orchard/internal/probe"
)

// SyncReady refuses to overwrite either artifact if the scan or probes are
// incomplete. A failed probe for an existing mapped model is ambiguous and
// needs review; it is not evidence that the model has been retired.
func (p *Pipeline) SyncReady(previous map[string]string) error {
	if p.Probes == nil || p.Probes.Tripped {
		return fmt.Errorf("probes did not complete")
	}
	if len(p.Listings.Degraded) > 0 || !p.Router.Reachable {
		return fmt.Errorf("one or more model sources did not respond")
	}
	if len(p.Listings.ByRegion) != len(p.Cfg.Regions()) {
		return fmt.Errorf("bedrock discovery did not cover every configured region")
	}
	for _, region := range p.Cfg.Regions() {
		if _, ok := p.Listings.ByRegion[region]; !ok {
			return fmt.Errorf("bedrock discovery missing region %s", region)
		}
	}
	if len(p.InvocableBedrock()) == 0 || len(p.InvocableRouter()) == 0 {
		return fmt.Errorf("one provider has no verified models")
	}
	if len(p.RouterCandidates) != len(p.Router.Deployed) {
		return fmt.Errorf("router probe list does not cover deployment")
	}
	results := map[string]probe.Outcome{}
	for _, outcome := range p.Probes.Outcomes {
		if outcome.Status == probe.SKIP || outcome.Systemic {
			return fmt.Errorf("a probe was skipped or failed for a systemic reason")
		}
		key := outcome.Provider + "\x00" + outcome.ID
		if _, ok := results[key]; !ok || outcome.Status == probe.OK {
			results[key] = outcome
		}
	}
	var unprobed []string
	for id := range p.BedrockCandidates {
		if _, ok := results[model.Bedrock+"\x00"+id]; !ok {
			unprobed = append(unprobed, id)
		}
	}
	for _, id := range p.RouterCandidates {
		outcome, ok := results[model.Router+"\x00"+id]
		if !ok {
			unprobed = append(unprobed, id)
			continue
		}
		if outcome.Status != probe.OK {
			return fmt.Errorf("router model %s did not pass its staged Pi probe", id)
		}
	}
	if len(unprobed) != 0 {
		sort.Strings(unprobed)
		return fmt.Errorf("models not probed: %s", strings.Join(unprobed, ", "))
	}
	for _, row := range p.Rows() {
		if row.Candidate == model.Yes && row.Invocable == model.Unknown {
			return fmt.Errorf("probe outcome for %s is uncertain", row.ID)
		}
	}
	var lost []string
	verified := p.InvocableBedrock()
	knownBedrock := p.CatalogIDs(model.Bedrock)
	for id := range previous {
		if !knownBedrock[id] {
			lost = append(lost, id)
			continue
		}
		if _, ok := verified[id]; !ok {
			lost = append(lost, id)
		}
	}
	if len(lost) != 0 {
		sort.Strings(lost)
		return fmt.Errorf("previously mapped models are unverified: %s", strings.Join(lost, ", "))
	}
	return nil
}
