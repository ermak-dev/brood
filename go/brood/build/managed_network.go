package build

import (
	"encoding/json"
	"errors"

	"brood.dev/go/brood/proxmox/egress"
)

type managedNetworkRule struct {
	Hosts []string `json:"hosts"`
	Ports []int    `json:"ports"`
}

func planManagedNetwork(document map[string]any, name string) (map[string]any, error) {
	networks, _ := asMap(document["networks"])
	selected, present := networks[name]
	if !present {
		return nil, errors.New("unknown managed network reference")
	}
	raw, err := json.Marshal(selected)
	if err != nil {
		return nil, errors.New("invalid managed network declaration")
	}
	var network struct {
		HTTP *managedNetworkRule `json:"http"`
		TLS  *managedNetworkRule `json:"tls"`
	}
	if json.Unmarshal(raw, &network) != nil {
		return nil, errors.New("invalid managed network declaration")
	}
	rules := []egress.Rule{}
	for _, entry := range []struct {
		protocol string
		rule     *managedNetworkRule
	}{{egress.HTTP, network.HTTP}, {egress.TLS, network.TLS}} {
		if entry.rule != nil {
			rules = append(rules, egress.Rule{Protocol: entry.protocol, Hostnames: entry.rule.Hosts, Ports: entry.rule.Ports})
		}
	}
	policy, err := egress.Compile(rules)
	if err != nil {
		return nil, err
	}
	encoded, err := policy.Encode()
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if json.Unmarshal(encoded, &result) != nil {
		return nil, errors.New("could not encode managed network declaration")
	}
	return result, nil
}
