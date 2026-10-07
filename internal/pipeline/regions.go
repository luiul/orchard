package pipeline

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/luiul/orchard/internal/bedrock"
	"github.com/luiul/orchard/internal/paths"
)

// bedrockProbeRegions tries catalog models listed by AWS, matched by the
// curated scope, or recorded in the prior region map. Models missing from
// discovery get bounded attempts in all scanned regions. The saved map
// provides a first region to try, not proof that a model still works.
func bedrockProbeRegions(cfg paths.Config, listings bedrock.Listings, catalog, selectedIDs map[string]bool, overrides map[string]string) (map[string][]string, error) {
	regions := cfg.Regions()
	configured := map[string]bool{}
	for _, region := range regions {
		configured[region] = true
	}
	for id, region := range overrides {
		if !configured[region] {
			return nil, fmt.Errorf("probeRegionOverrides[%q]: %q is not a scanned region", id, region)
		}
	}

	assigned := bedrock.AssignCandidates(listings.ByRegion, catalog, regions, cfg.DefaultRegion, overrides)
	var saved struct {
		Models map[string]string `json:"models"`
	}
	if cfg.BedrockModelsJSON != "" {
		if data, err := os.ReadFile(cfg.BedrockModelsJSON); err == nil {
			// A malformed old map cannot silently steer a new probe.
			if err := json.Unmarshal(data, &saved); err != nil {
				return nil, fmt.Errorf("read saved Bedrock region map: %w", err)
			}
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read saved Bedrock region map: %w", err)
		}
	}

	out := make(map[string][]string)
	for id := range catalog {
		preferred, discovered := assigned[id]
		selected := selectedIDs[id]
		_, previouslyMapped := saved.Models[id]
		if !discovered && !selected && !previouslyMapped {
			continue
		}
		add := func(region string) {
			if !configured[region] {
				return
			}
			for _, existing := range out[id] {
				if existing == region {
					return
				}
			}
			out[id] = append(out[id], region)
		}
		if discovered {
			if listings.ByRegion[overrides[id]][id] {
				add(overrides[id])
			}
			if listings.ByRegion[preferred][id] {
				add(preferred)
			}
			for _, region := range regions {
				if listings.ByRegion[region][id] {
					add(region)
				}
			}
			// A selected or previously mapped model is worth checking even
			// where AWS discovery did not list it. Only a live probe proves it.
			if selected || previouslyMapped {
				add(saved.Models[id])
				for _, region := range regions {
					add(region)
				}
			}
		} else {
			add(overrides[id])
			add(saved.Models[id])
			for _, region := range regions {
				add(region)
			}
		}
	}
	return out, nil
}
