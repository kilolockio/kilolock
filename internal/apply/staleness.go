package apply

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/kilolockio/kilolock/internal/slice"
)

func changedReadSetAddresses(baseRaw, currentRaw []byte, readSet []string) ([]string, error) {
	if len(readSet) == 0 {
		return nil, nil
	}
	base, err := slice.ParseTrunkState(baseRaw)
	if err != nil {
		return nil, fmt.Errorf("parse base state: %w", err)
	}
	cur, err := slice.ParseTrunkState(currentRaw)
	if err != nil {
		return nil, fmt.Errorf("parse current state: %w", err)
	}

	baseByAddr := indexResourcesByAddress(base.Resources)
	curByAddr := indexResourcesByAddress(cur.Resources)

	groups := slice.IndexFootprintByGroup(readSet)
	var changed []string
	for addr := range groups {
		br, bok := baseByAddr[addr]
		cr, cok := curByAddr[addr]
		if !bok && !cok {
			continue
		}
		if bok != cok {
			changed = append(changed, addr)
			continue
		}
		if br.Module != cr.Module || br.Mode != cr.Mode || br.Type != cr.Type || br.Name != cr.Name || br.Provider != cr.Provider || br.Each != cr.Each {
			changed = append(changed, addr)
			continue
		}
		eq, err := jsonRawEqual(json.RawMessage(br.Instances), json.RawMessage(cr.Instances))
		if err != nil || !eq {
			changed = append(changed, addr)
			continue
		}
	}
	sort.Strings(changed)
	return changed, nil
}
