-- +goose Up
-- The foreground checkpoint asks whether one coordinator has any open request
-- ready for selection. Keep that probe account/realm/agent scoped even in a
-- realm with a large historical request volume. expires_at comes before the
-- branch-specific offer_deadline because live expiry is required on every
-- candidate row considered by the probe.
CREATE INDEX agent_message_requests_open_by_coordinator
    ON agent_message_requests
       (account_id, realm_id, coordinator_agent_id, expires_at, offer_deadline, id)
    WHERE state = 'open';

-- Once every candidate has answered, the coordinator probe must prove that no
-- pending response remains. The request primary key can find the snapshot, but
-- would still scan every candidate in the important all-answered case.
CREATE INDEX agent_message_request_candidates_pending_by_request
    ON agent_message_request_candidates (request_id)
    WHERE response_state = 'pending';

-- +goose Down
DROP INDEX agent_message_request_candidates_pending_by_request;
DROP INDEX agent_message_requests_open_by_coordinator;
