package event

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// TypePrefix is prepended to the kind to form the CloudEvents "type".
const TypePrefix = "changefeed."

// CloudEvent is the structured-mode JSON envelope accepted by the API.
// See https://github.com/cloudevents/spec/blob/v1.0.2/cloudevents/spec.md
type CloudEvent struct {
	SpecVersion string    `json:"specversion"`
	ID          string    `json:"id,omitempty"`
	Type        string    `json:"type"`
	Source      string    `json:"source"`
	Subject     string    `json:"subject"`
	Time        time.Time `json:"time,omitzero"`
	Data        Data      `json:"data"`
}

// Data is the CloudEvent payload.
type Data struct {
	Env       string            `json:"env"`
	Summary   string            `json:"summary"`
	Actor     string            `json:"actor,omitempty"`
	Risk      Risk              `json:"risk,omitempty"`
	Revision  string            `json:"revision,omitempty"`
	ChangeSet string            `json:"changeset,omitempty"`
	OutsideCI bool              `json:"outsideCI,omitempty"`
	Diff      []string          `json:"diff,omitempty"`
	Links     map[string]string `json:"links,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// FromCloudEvent converts an envelope into an Event. It does not validate;
// call Normalize afterwards.
func FromCloudEvent(ce CloudEvent) (Event, error) {
	if ce.SpecVersion != "1.0" {
		return Event{}, fmt.Errorf("specversion must be \"1.0\"")
	}
	kind, ok := strings.CutPrefix(ce.Type, TypePrefix)
	if !ok {
		return Event{}, fmt.Errorf("type must look like %q", TypePrefix+"deploy")
	}
	lane, origin, _ := strings.Cut(ce.Source, "/")
	return Event{
		ID: ce.ID, Time: ce.Time, Source: lane, Origin: origin, Kind: Kind(kind),
		Service: ce.Subject, Env: ce.Data.Env, Actor: ce.Data.Actor, Summary: ce.Data.Summary,
		Risk: ce.Data.Risk, Revision: ce.Data.Revision, ChangeSet: ce.Data.ChangeSet,
		OutsideCI: ce.Data.OutsideCI, Diff: ce.Data.Diff, Links: ce.Data.Links, Labels: ce.Data.Labels,
	}, nil
}

// ToCloudEvent converts an Event into its envelope.
func (e Event) ToCloudEvent() CloudEvent {
	src := e.Source
	if e.Origin != "" {
		src += "/" + e.Origin
	}
	return CloudEvent{
		SpecVersion: "1.0", ID: e.ID, Type: TypePrefix + string(e.Kind), Source: src, Subject: e.Service, Time: e.Time,
		Data: Data{Env: e.Env, Summary: e.Summary, Actor: e.Actor, Risk: e.Risk, Revision: e.Revision,
			ChangeSet: e.ChangeSet, OutsideCI: e.OutsideCI, Diff: e.Diff, Links: e.Links, Labels: e.Labels},
	}
}

// DecodeBatch accepts a single CloudEvent object or an array of them.
func DecodeBatch(raw []byte) ([]CloudEvent, error) {
	trimmed := strings.TrimLeft(string(raw), " \t\r\n")
	if strings.HasPrefix(trimmed, "[") {
		var list []CloudEvent
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("decode event array: %w", err)
		}
		return list, nil
	}
	var one CloudEvent
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, fmt.Errorf("decode event: %w", err)
	}
	return []CloudEvent{one}, nil
}
