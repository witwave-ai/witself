package transcriptcapture

import "errors"

// EventBindingError verifies the queued event belongs to the installed binding.
// Stable IDs survive display-name changes. Legacy account/realm names are
// accepted only when the event's agent and location IDs pin its original scope.
func EventBindingError(event Event, cfg Config) error {
	legacyEventContext := event.AccountID == "" && event.RealmID == "" &&
		event.AgentID != "" && event.AgentID == cfg.AgentID &&
		event.Location.ID != "" && event.Location.ID == cfg.Location.ID
	accountMatches := stableCaptureIdentityMatches(event.AccountID, cfg.AccountID, event.Account, cfg.Account, legacyEventContext)
	realmMatches := stableCaptureIdentityMatches(event.RealmID, cfg.RealmID, event.Realm, cfg.Realm, legacyEventContext)
	agentMatches := stableCaptureIdentityMatches(event.AgentID, cfg.AgentID,
		event.Agent+"\x00"+event.AgentName, cfg.Agent+"\x00"+cfg.AgentName, false)
	if event.Runtime != cfg.Runtime || !accountMatches || !realmMatches || !agentMatches || event.Location.ID != cfg.Location.ID {
		return errors.New("queued transcript identity does not match the installed runtime binding")
	}
	return nil
}

func stableCaptureIdentityMatches(eventID, configID, eventName, configName string, allowLegacyEvent bool) bool {
	if eventID != "" && configID != "" {
		return eventID == configID
	}
	if eventID == "" && configID == "" {
		return eventName == configName
	}
	if eventID == "" && configID != "" && allowLegacyEvent {
		return eventName == configName
	}
	return false
}
