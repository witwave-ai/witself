package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// seedOptions describes row counts, never production identifiers or values.
type seedOptions struct {
	Entries     int64
	Transcripts int64
	Mutations   int64
	Inputs      int64
	OtherRows   int64
}

func addSeedFlags(fs *flag.FlagSet, opts *seedOptions) {
	fs.Int64Var(&opts.Entries, "entries", 330304, "synthetic transcript entry count")
	fs.Int64Var(&opts.Transcripts, "transcripts", 2000, "synthetic transcript count")
	fs.Int64Var(&opts.Mutations, "mutations", 174212, "synthetic curation mutation count")
	fs.Int64Var(&opts.Inputs, "inputs", 30225, "synthetic curation input count")
	fs.Int64Var(&opts.OtherRows, "other-rows", 300, "synthetic supporting table row count")
}

type seedMeasurements struct {
	Entries         int64 `json:"entries"`
	Transcripts     int64 `json:"transcripts"`
	Mutations       int64 `json:"mutations"`
	Inputs          int64 `json:"inputs"`
	Requests        int64 `json:"requests"`
	HeapBytes       int64 `json:"heap_bytes"`
	TotalBytes      int64 `json:"total_bytes"`
	ColumnBytes     int64 `json:"column_bytes"`
	AllColumnBytes  int64 `json:"all_column_bytes"`
	PayloadRows     int64 `json:"payload_rows"`
	ReplyRows       int64 `json:"reply_rows"`
	BodyMinBytes    int64 `json:"body_min_bytes"`
	BodyMedianBytes int64 `json:"body_median_bytes"`
	BodyMaxBytes    int64 `json:"body_max_bytes"`
}

func validateSeedOptions(opts seedOptions) error {
	if opts.Entries < 3 || opts.Transcripts < 3 || opts.Transcripts > opts.Entries ||
		opts.Mutations < 1 || opts.Inputs < 1 || opts.OtherRows < 3 || opts.OtherRows > 10000 {
		return errors.New("synthetic shape requires entries >= transcripts >= 3, positive mutations/inputs, and other-rows in [3,10000]")
	}
	if (opts.Inputs+opts.OtherRows-1)/opts.OtherRows > 10000 {
		return errors.New("synthetic shape exceeds the per-run curation input limit")
	}
	return nil
}

// seedSynthetic keeps all generation in PostgreSQL. The fixed dictionary contains
// pseudorandom, non-personal bytes rather than repeat('x', n), which TOAST would
// compress to a misleadingly tiny fixture. No storage parameters or indexes are
// changed to force the size acceptance; the caller must check actual sizes.
func seedSynthetic(ctx context.Context, db *sql.DB, opts seedOptions) (seedMeasurements, error) {
	if err := validateSeedOptions(opts); err != nil {
		return seedMeasurements{}, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return seedMeasurements{}, errors.New("open synthetic connection")
	}
	defer func() { _ = conn.Close() }()
	var populated bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM accounts)`).Scan(&populated); err != nil {
		return seedMeasurements{}, errors.New("inspect synthetic destination")
	}
	if populated {
		return seedMeasurements{}, errors.New("synthetic destination must be empty")
	}
	exec := func(label, query string, args ...any) error {
		// A fixed multi-statement program is intentional here. pgx's simple
		// protocol safely interpolates only these numeric shape arguments.
		params := append([]any{pgx.QueryExecModeSimpleProtocol}, args...)
		if _, err := conn.ExecContext(ctx, query, params...); err != nil {
			// Driver errors may include whole tuples. Deliberately retain only
			// a static phase name in a user-visible diagnostic.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && seedDiagnosticPattern.MatchString(pgErr.Code) {
				return fmt.Errorf("synthetic generation failed: %s (SQLSTATE %s)", label, pgErr.Code)
			}
			return fmt.Errorf("synthetic generation failed: %s", label)
		}
		return nil
	}
	if err := exec("dictionary", `
CREATE TEMP TABLE rehearsal_entropy AS
SELECT d, string_agg(md5(d::text || ':' || n::text), '' ORDER BY n) AS content
FROM generate_series(0,63) d CROSS JOIN generate_series(1,2048) n GROUP BY d;
CREATE TEMP TABLE rehearsal_bodies AS
SELECT d, shape.bytes AS body_bytes, substr(content,1,shape.bytes) AS body
FROM rehearsal_entropy CROSS JOIN (VALUES (0),(512),(1024),(1408),(8192),(65536)) shape(bytes);
CREATE UNIQUE INDEX ON rehearsal_bodies(d,body_bytes);
CREATE TEMP TABLE rehearsal_payloads AS
SELECT d, shape.bytes AS payload_bytes,
 jsonb_build_object('data',substr(content,16385,shape.bytes-12)) AS payload
FROM rehearsal_entropy CROSS JOIN (VALUES (2048),(2650),(8192),(16384)) shape(bytes);
CREATE UNIQUE INDEX ON rehearsal_payloads(d,payload_bytes);
DROP TABLE rehearsal_entropy;
ANALYZE rehearsal_bodies;
ANALYZE rehearsal_payloads;
`+syntheticIDSQL); err != nil {
		return seedMeasurements{}, err
	}
	if err := exec("parents", `
INSERT INTO accounts(id,display_name)
 SELECT pg_temp.rehearsal_id('acc_',n), 'Synthetic' FROM generate_series(1,3) n;
INSERT INTO realms(id,account_id,name)
 SELECT pg_temp.rehearsal_id('realm_',n), pg_temp.rehearsal_id('acc_',((n-1)%3+1)), 'synthetic_'||n
 FROM generate_series(1,$1::bigint) n;
INSERT INTO agents(id,realm_id,name)
 SELECT pg_temp.rehearsal_id('agent_',n), pg_temp.rehearsal_id('realm_',n), 'synthetic_'||n
 FROM generate_series(1,$1::bigint) n;
INSERT INTO operators(id,account_id,role)
 SELECT pg_temp.rehearsal_id('opr_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),'account_owner'
 FROM generate_series(1,$1::bigint) n;`, opts.OtherRows); err != nil {
		return seedMeasurements{}, err
	}
	if err := exec("transcripts", `
WITH shape AS (
 SELECT n, CASE WHEN n <= greatest(1,($1::bigint*90)/100) THEN 1
 WHEN n <= greatest(2,($1::bigint*98)/100) THEN 2 ELSE 3 END AS a
 FROM generate_series(1,$1::bigint) n)
INSERT INTO transcript_conversations(id,account_id,realm_id,owner_agent_id,next_sequence)
 SELECT pg_temp.rehearsal_id('trn_',n), pg_temp.rehearsal_id('acc_',a),
 pg_temp.rehearsal_id('realm_',a), pg_temp.rehearsal_id('agent_',a), ($2::bigint-n)/$1::bigint+2 FROM shape;`, opts.Transcripts, opts.Entries); err != nil {
		return seedMeasurements{}, err
	}
	// Bound each statement's immediate/deferred FK work and memory use. Every
	// insert still uses generate_series; no content crosses the client boundary.
	const batchSize int64 = 5000
	for first := int64(1); first <= opts.Entries; first += batchSize {
		last := min(first+batchSize-1, opts.Entries)
		if err := exec("entries", syntheticEntriesSQL, first, last, opts.Transcripts); err != nil {
			return seedMeasurements{}, err
		}
	}
	if err := exec("curation", syntheticCurationSQL, opts.OtherRows, min(int64(30000), opts.Mutations), opts.Inputs); err != nil {
		return seedMeasurements{}, err
	}
	for first := int64(1); first <= opts.Mutations; first += batchSize {
		last := min(first+batchSize-1, opts.Mutations)
		if err := exec("mutations", syntheticMutationsSQL, first, last, min(int64(30000), opts.Mutations)); err != nil {
			return seedMeasurements{}, err
		}
	}
	if err := exec("supporting rows", syntheticSupportingSQL, opts.OtherRows); err != nil {
		return seedMeasurements{}, err
	}
	if err := exec("supporting graph", syntheticGraphSQL, opts.OtherRows); err != nil {
		return seedMeasurements{}, err
	}
	if err := exec("lifecycle graph", syntheticLifecycleSQL, opts.OtherRows); err != nil {
		return seedMeasurements{}, err
	}
	if err := exec("email graph", syntheticEmailSQL, opts.OtherRows); err != nil {
		return seedMeasurements{}, err
	}
	if err := exec("analyze", `ANALYZE`); err != nil {
		return seedMeasurements{}, err
	}
	// Release the pinned connection before measuring: tests and callers may
	// deliberately permit only one connection in the pool.
	if err := conn.Close(); err != nil {
		return seedMeasurements{}, errors.New("close synthetic connection")
	}
	return measureSynthetic(ctx, db, opts)
}

// IDs have the production prefix plus sixteen base32 characters. Scope IDs are
// shared across all seed programs, so widening a key also widens every FK value.
const syntheticIDSQL = `
CREATE FUNCTION pg_temp.rehearsal_id(prefix text, n bigint) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
 SELECT prefix || translate(lpad(to_hex(n),16,'0'),'0123456789','ghijklmnop')
$$;`

var seedDiagnosticPattern = regexp.MustCompile(`^[A-Z0-9]{5}$`)

const syntheticEntriesSQL = `
WITH shape AS (
 SELECT n, ((n-1)%$3::bigint)+1 AS t, ((n-1)/$3::bigint)+1 AS seq,
 CASE WHEN (n/10)%1000 = 0 AND n%10 < 6 THEN 0
      WHEN (n/10)%1000 < 10 THEN 512
      WHEN (n/10)%1000 < 520 THEN 1024
      WHEN (n/10)%1000 < 990 THEN 1408
      WHEN (n/10)%1000 < 999 THEN 8192 ELSE 65536 END AS body_bytes,
 CASE WHEN (n/10)%100 = 0 THEN 2048
      WHEN (n/10)%100 < 85 THEN 2650
      WHEN (n/10)%100 < 95 THEN 8192 ELSE 16384 END AS payload_bytes
 FROM generate_series($1::bigint,$2::bigint) n)
INSERT INTO transcript_entries
 (id,account_id,transcript_id,realm_id,recorded_by_agent_id,sequence,role,body,payload,reply_to_entry_id,artifacts)
 SELECT pg_temp.rehearsal_id('ent_',s.n),c.account_id,c.id,c.realm_id,c.owner_agent_id,s.seq,
 CASE WHEN s.n%2=0 THEN 'assistant' ELSE 'user' END,
 b.body,p.payload,
 CASE WHEN s.n>$3::bigint THEN pg_temp.rehearsal_id('ent_',s.n-$3::bigint) END,'[]'::jsonb
 FROM shape s JOIN transcript_conversations c ON c.id=pg_temp.rehearsal_id('trn_',s.t)
 JOIN rehearsal_bodies b ON b.d=s.n%64 AND b.body_bytes=s.body_bytes
 LEFT JOIN rehearsal_payloads p ON p.d=s.n%64 AND p.payload_bytes=s.payload_bytes AND s.n%10 < 6
 ORDER BY s.n;`

const syntheticCurationSQL = `
INSERT INTO memory_curation_lanes(account_id,realm_id,owner_id)
 SELECT pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),pg_temp.rehearsal_id('agent_',n)
 FROM generate_series(1,$1::bigint) n;
INSERT INTO memory_curation_requests
 (id,account_id,realm_id,owner_id,coalescing_key,trigger_reason,request_generation,actor_id,idempotency_key,request_hash)
 SELECT pg_temp.rehearsal_id('mcrq_',n),pg_temp.rehearsal_id('acc_',1),pg_temp.rehearsal_id('realm_',1),pg_temp.rehearsal_id('agent_',1),
 'synthetic_'||n,'manual',1,pg_temp.rehearsal_id('agent_',1),'synthetic_request_'||n,repeat('a',64)
 FROM generate_series(1,greatest($1::bigint,$2::bigint)) n;
INSERT INTO memory_curation_runs
 (id,account_id,realm_id,owner_id,request_id,request_generation,fencing_generation,state,
 actor_id,idempotency_key,request_hash,terminal_at,input_count,transcript_input_count)
 SELECT pg_temp.rehearsal_id('mrun_',n),pg_temp.rehearsal_id('acc_',1),pg_temp.rehearsal_id('realm_',1),pg_temp.rehearsal_id('agent_',1),
 pg_temp.rehearsal_id('mcrq_',n),1,n,'abandoned',pg_temp.rehearsal_id('agent_',1),
 'synthetic_run_'||n,repeat('b',64),now(),
 CASE WHEN n>$3::bigint THEN 0 ELSE ($3::bigint-n)/$1::bigint+1 END,
 CASE WHEN n>$3::bigint THEN 0 ELSE ($3::bigint-n)/$1::bigint+1 END
 FROM generate_series(1,$1::bigint) n;
INSERT INTO memory_curation_run_inputs
 (run_id,ordinal,account_id,realm_id,owner_id,input_kind,order_key,transcript_id,sequence_from,sequence_until)
 SELECT pg_temp.rehearsal_id('mrun_',((n-1)%$1::bigint)+1),((n-1)/$1::bigint)+1,
 pg_temp.rehearsal_id('acc_',1),pg_temp.rehearsal_id('realm_',1),pg_temp.rehearsal_id('agent_',1),'transcript','synthetic_'||n,
 pg_temp.rehearsal_id('trn_',1),1,1 FROM generate_series(1,$3::bigint) n;
INSERT INTO memory_curation_actions
 (id,run_id,account_id,realm_id,owner_id,ordinal,plan_revision,primitive)
 SELECT pg_temp.rehearsal_id('mact_',n),pg_temp.rehearsal_id('mrun_',n),
 pg_temp.rehearsal_id('acc_',1),pg_temp.rehearsal_id('realm_',1),pg_temp.rehearsal_id('agent_',1),1,1,'create'
 FROM generate_series(1,$1::bigint) n;`

const syntheticMutationsSQL = `
INSERT INTO memory_curation_mutations
 (id,account_id,realm_id,owner_id,actor_id,operation,idempotency_key,request_hash,request_id,request_generation,result_state)
 SELECT pg_temp.rehearsal_id('mcmu_',n),pg_temp.rehearsal_id('acc_',1),pg_temp.rehearsal_id('realm_',1),pg_temp.rehearsal_id('agent_',1),
 pg_temp.rehearsal_id('agent_',1),'request','synthetic_mutation_'||n,repeat('c',64),
 pg_temp.rehearsal_id('mcrq_',((n-1)%$3::bigint)+1),1,'queued'
 FROM generate_series($1::bigint,$2::bigint) n;`

const syntheticSupportingSQL = `
INSERT INTO usage_events
 (id,account_id,realm_id,agent_id,dimension,quantity,unit,subject_type,subject_id,idempotency_key,occurred_at)
 SELECT pg_temp.rehearsal_id('usg_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),'synthetic',1,'entries','synthetic','synthetic','synthetic_usage_'||n,now()
 FROM generate_series(1,$1::bigint) n;
INSERT INTO fact_subjects(id,account_id,realm_id,owner_agent_id,canonical_key)
 SELECT pg_temp.rehearsal_id('sub_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),'synthetic' FROM generate_series(1,$1::bigint) n;
INSERT INTO facts(id,account_id,realm_id,owner_agent_id,subject_id,predicate)
 SELECT pg_temp.rehearsal_id('fact_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),pg_temp.rehearsal_id('sub_',n),'synthetic/value' FROM generate_series(1,$1::bigint) n;
BEGIN;
INSERT INTO memories(id,account_id,realm_id,owner_id,origin,authored_by_agent_id,current_version)
 SELECT pg_temp.rehearsal_id('mem_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),'synthetic',pg_temp.rehearsal_id('agent_',n),1 FROM generate_series(1,$1::bigint) n;
INSERT INTO memory_versions
 (memory_id,version,account_id,realm_id,owner_id,change_seq,content,kind,content_hash,actor_id,operation,idempotency_key,request_hash)
 SELECT pg_temp.rehearsal_id('mem_',n),1,pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),1,'Synthetic rehearsal data','note',repeat('d',64),pg_temp.rehearsal_id('agent_',n),
 'added','synthetic_memory_'||n,repeat('e',64) FROM generate_series(1,$1::bigint) n;
INSERT INTO memory_evidence
 (id,account_id,realm_id,owner_id,memory_id,target_version,evidence_change_seq,
 evidence_type,resolution_state,external_locator,actor_id)
 SELECT pg_temp.rehearsal_id('mev_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),pg_temp.rehearsal_id('mem_',n),1,1,'synthetic','pending',
 'synthetic',pg_temp.rehearsal_id('agent_',n) FROM generate_series(1,$1::bigint) n;
COMMIT;`

const syntheticGraphSQL = `
INSERT INTO tokens(id,account_id,operator_id,kind,token_hash)
 SELECT pg_temp.rehearsal_id('tok_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('opr_',n),
 'operator',md5('synthetic-token-'||n)||md5('synthetic-token-tail-'||n)
 FROM generate_series(1,$1::bigint) n;
INSERT INTO fact_assertions
 (id,fact_id,account_id,realm_id,asserted_by_agent_id,value_type,value,source_kind,observed_at)
 SELECT pg_temp.rehearsal_id('fas_',n),pg_temp.rehearsal_id('fact_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),
 pg_temp.rehearsal_id('realm_',n),pg_temp.rehearsal_id('agent_',n),'text','"synthetic"'::jsonb,'agent',now()
 FROM generate_series(1,$1::bigint) n;
INSERT INTO fact_candidates
 (id,account_id,realm_id,owner_agent_id,subject_key,predicate,value_type,value,observed_at)
 SELECT pg_temp.rehearsal_id('fcand_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),'synthetic','synthetic/value','text','"synthetic"'::jsonb,now()
 FROM generate_series(1,$1::bigint) n;
INSERT INTO memory_change_clocks(account_id,realm_id,owner_id,last_change_seq)
 SELECT pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),pg_temp.rehearsal_id('agent_',n),1
 FROM generate_series(1,$1::bigint) n;
INSERT INTO memory_vector_profiles
 (id,account_id,realm_id,owner_id,provider,model,recipe,recipe_version,dimensions,
 distance_metric,normalization,contract_hash,created_by_agent_id)
 SELECT pg_temp.rehearsal_id('mvp_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),'synthetic','synthetic','synthetic','v1',1,'cosine','none',
 repeat('a',64),pg_temp.rehearsal_id('agent_',n) FROM generate_series(1,$1::bigint) n;
INSERT INTO memory_vectors
 (profile_id,memory_id,memory_version,account_id,realm_id,owner_id,content_hash,vector,vector_hash,created_by_agent_id)
 SELECT pg_temp.rehearsal_id('mvp_',n),pg_temp.rehearsal_id('mem_',n),1,
 pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),pg_temp.rehearsal_id('agent_',n),
 repeat('d',64),'[1]'::jsonb,repeat('b',64),pg_temp.rehearsal_id('agent_',n) FROM generate_series(1,$1::bigint) n;
INSERT INTO memory_curation_cursors(account_id,realm_id,owner_id,source_kind,source_stream_id)
 SELECT pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),pg_temp.rehearsal_id('agent_',n),
 'memory','synthetic' FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_dashboard_preferences(agent_id,account_id,realm_id,prefs)
 SELECT pg_temp.rehearsal_id('agent_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 '{"schema":"witself.dashboard-prefs.v1","theme":"system"}'::jsonb FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_messages(id,account_id,realm_id,from_agent_id,to_agent_id,body,thread_id)
 SELECT pg_temp.rehearsal_id('msg_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),pg_temp.rehearsal_id('agent_',n),'Synthetic rehearsal data',pg_temp.rehearsal_id('thr_',n)
 FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_message_deliveries(message_id,account_id,realm_id,recipient_agent_id,delivered_at)
 SELECT pg_temp.rehearsal_id('msg_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),now() FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_vault_keys
 (id,account_id,realm_id,owner_agent_id,key_version,algorithm,fingerprint)
 SELECT pg_temp.rehearsal_id('avk_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),1,'AES_256_GCM_RANDOM_NONCE_V1',md5('synthetic-key-'||n)||md5('synthetic-key-tail-'||n)
 FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_vault_key_enrollments
 (id,account_id,realm_id,owner_agent_id,vault_key_id,vault_key_version,target_location_id,
 target_public_key,target_key_algorithm,pairing_commitment,expires_at)
 SELECT pg_temp.rehearsal_id('enr_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),pg_temp.rehearsal_id('avk_',n),1,pg_temp.rehearsal_id('loc_',n),
 repeat('a',43),'X25519_RAW_32_BASE64URL_V1',repeat('a',64),now()+interval '1 day'
 FROM generate_series(1,$1::bigint) n;
INSERT INTO secrets(id,account_id,realm_id,owner_agent_id,name)
 SELECT pg_temp.rehearsal_id('sec_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),'Synthetic' FROM generate_series(1,$1::bigint) n;
INSERT INTO secret_fields(id,account_id,realm_id,owner_agent_id,secret_id,name,sensitive,public_value)
 SELECT pg_temp.rehearsal_id('fld_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),pg_temp.rehearsal_id('sec_',n),'synthetic',false,'Synthetic'
 FROM generate_series(1,$1::bigint) n;`

const syntheticLifecycleSQL = `
INSERT INTO agent_message_requests
 (id,account_id,realm_id,opening_message_id,coordinator_agent_id,offer_window_seconds,
 expires_in_seconds,offer_deadline,expires_at,selection_generation)
 SELECT pg_temp.rehearsal_id('mrq_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('msg_',n),pg_temp.rehearsal_id('agent_',n),60,3600,now()+interval '1 minute',now()+interval '1 hour',1
 FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_message_request_candidates(request_id,account_id,realm_id,agent_id)
 SELECT pg_temp.rehearsal_id('mrq_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n) FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_message_request_selections
 (id,request_id,account_id,realm_id,coordinator_agent_id,generation,idempotency_key_hash,selection_hash)
 SELECT pg_temp.rehearsal_id('msel_',n),pg_temp.rehearsal_id('mrq_',n),
 pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),pg_temp.rehearsal_id('agent_',n),1,repeat('a',64),repeat('b',64)
 FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_message_request_claims
 (id,request_id,selection_id,account_id,realm_id,agent_id,lease_expires_at)
 SELECT pg_temp.rehearsal_id('mrc_',n),pg_temp.rehearsal_id('mrq_',n),pg_temp.rehearsal_id('msel_',n),
 pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),pg_temp.rehearsal_id('agent_',n),now()+interval '1 hour'
 FROM generate_series(1,$1::bigint) n;
INSERT INTO facts
 (id,account_id,realm_id,owner_agent_id,subject_id,predicate,deleted_at,deleted_by_agent_id,
 delete_receipt_id,delete_idempotency_key_hash,deleted_prior_assertion_id,deleted_assertion_count,deleted_candidate_revision)
 SELECT pg_temp.rehearsal_id('fact_',n+$1::bigint),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),pg_temp.rehearsal_id('sub_',n),'synthetic/deleted',now(),pg_temp.rehearsal_id('agent_',n),
 pg_temp.rehearsal_id('fdel_',n),repeat('a',64),pg_temp.rehearsal_id('fas_',n+$1::bigint),1,repeat('b',64)
 FROM generate_series(1,$1::bigint) n;
INSERT INTO fact_mutation_tombstones
 (id,account_id,realm_id,owner_agent_id,fact_id,surface,idempotency_key_hash,deleted_at)
 SELECT pg_temp.rehearsal_id('fmt_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),pg_temp.rehearsal_id('fact_',n+$1::bigint),'set',repeat('a',64),now()
 FROM generate_series(1,$1::bigint) n;
BEGIN;
INSERT INTO memories(id,account_id,realm_id,owner_id,origin,authored_by_agent_id,current_version)
 SELECT pg_temp.rehearsal_id('mem_',n+$1::bigint),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),'synthetic',pg_temp.rehearsal_id('agent_',n),1 FROM generate_series(1,$1::bigint) n;
INSERT INTO memory_versions
 (memory_id,version,account_id,realm_id,owner_id,change_seq,content,kind,content_hash,actor_id,operation,idempotency_key,request_hash)
 SELECT pg_temp.rehearsal_id('mem_',n+$1::bigint),1,pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),2,'Synthetic replacement','note',repeat('d',64),pg_temp.rehearsal_id('agent_',n),
 'added','synthetic_replacement_'||n,repeat('e',64) FROM generate_series(1,$1::bigint) n;
INSERT INTO memory_evidence
 (id,account_id,realm_id,owner_id,memory_id,target_version,evidence_change_seq,
 evidence_type,resolution_state,external_locator,actor_id)
 SELECT pg_temp.rehearsal_id('mev_',n+$1::bigint),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),pg_temp.rehearsal_id('mem_',n+$1::bigint),1,2,'synthetic','pending',
 'synthetic',pg_temp.rehearsal_id('agent_',n) FROM generate_series(1,$1::bigint) n;
UPDATE memory_versions v SET state='superseded',operation='superseded',
 supersession_set_id=pg_temp.rehearsal_id('mset_',n),supersession_set_revision=1,
 supersession_replacement_count=1,supersession_replacement_digest=repeat('a',64)
 FROM generate_series(1,$1::bigint) n WHERE v.memory_id=pg_temp.rehearsal_id('mem_',n);
INSERT INTO memory_relations
 (id,account_id,realm_id,owner_id,from_memory_id,from_version,to_memory_id,to_version,
 relation_type,supersession_set_id,supersession_set_revision)
 SELECT pg_temp.rehearsal_id('mrel_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),pg_temp.rehearsal_id('mem_',n+$1::bigint),1,pg_temp.rehearsal_id('mem_',n),1,
 'supersedes',pg_temp.rehearsal_id('mset_',n),1 FROM generate_series(1,$1::bigint) n;
COMMIT;
INSERT INTO memories
 (id,account_id,realm_id,owner_id,origin,authored_by_agent_id,permanently_deleted_at,
 permanently_deleted_by_id,permanent_delete_reason,delete_receipt_id,delete_idempotency_key_hash,
 deleted_prior_version,deleted_scrub_set_revision,deleted_version_count,deleted_evidence_count,
 deleted_retry_shield_count,deleted_retry_shield_digest)
 SELECT pg_temp.rehearsal_id('mem_',n+2*$1::bigint),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),'synthetic',pg_temp.rehearsal_id('agent_',n),now(),pg_temp.rehearsal_id('agent_',n),'synthetic',
 pg_temp.rehearsal_id('mdel_',n),repeat('a',64),1,repeat('b',64),1,1,1,repeat('c',64)
 FROM generate_series(1,$1::bigint) n;
INSERT INTO memory_deleted_references
 (id,account_id,realm_id,owner_id,deleted_memory_id,former_reference_kind,related_resource_id,reason_code)
 SELECT pg_temp.rehearsal_id('mdr_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),pg_temp.rehearsal_id('mem_',n+2*$1::bigint),'idempotency.added',repeat('a',64),'permanent_delete'
 FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_vault_keys
 (id,account_id,realm_id,owner_agent_id,key_version,algorithm,fingerprint,lifecycle_state)
 SELECT pg_temp.rehearsal_id('avk_',n+$1::bigint),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),2,'AES_256_GCM_RANDOM_NONCE_V1',md5('pending-key-'||n)||md5('pending-key-tail-'||n),'pending'
 FROM generate_series(1,$1::bigint) n;
BEGIN;
INSERT INTO secret_fields
 (id,account_id,realm_id,owner_agent_id,secret_id,name,sensitive,value_encoding,
 envelope_version,ciphertext,aead_algorithm,aad_version,dek_id,dek_generation)
 SELECT pg_temp.rehearsal_id('fld_',n+$1::bigint),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),pg_temp.rehearsal_id('sec_',n),'synthetic_envelope',true,'binary',1,
 decode(repeat('ab',60),'hex'),'AES_256_GCM_RANDOM_NONCE_V1',1,pg_temp.rehearsal_id('dek_',n),1
 FROM generate_series(1,$1::bigint) n;
INSERT INTO secret_deks
 (id,account_id,realm_id,owner_agent_id,secret_id,field_id,dek_generation,wrapped_dek,
 wrap_algorithm,aad_version,wrapping_key_id,wrapping_key_version)
 SELECT pg_temp.rehearsal_id('dek_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),pg_temp.rehearsal_id('sec_',n),pg_temp.rehearsal_id('fld_',n+$1::bigint),1,
 decode(repeat('ab',60),'hex'),'AES_256_GCM_RANDOM_NONCE_V1',1,pg_temp.rehearsal_id('avk_',n),1
 FROM generate_series(1,$1::bigint) n;
COMMIT;
INSERT INTO agent_vault_key_rotations
 (id,account_id,realm_id,owner_agent_id,source_key_id,source_key_version,target_key_id,target_key_version,item_count)
 SELECT pg_temp.rehearsal_id('vkr_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),pg_temp.rehearsal_id('avk_',n),1,pg_temp.rehearsal_id('avk_',n+$1::bigint),2,1
 FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_vault_key_rotation_items
 (rotation_id,account_id,realm_id,owner_agent_id,secret_id,field_id,dek_id,dek_generation,
 source_dek_row_version,source_wrap_revision,source_wrapped_dek,source_wrap_algorithm,
 source_aad_version,source_wrapping_key_id,source_wrapping_key_version)
 SELECT pg_temp.rehearsal_id('vkr_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),pg_temp.rehearsal_id('sec_',n),pg_temp.rehearsal_id('fld_',n+$1::bigint),
 pg_temp.rehearsal_id('dek_',n),1,1,1,decode(repeat('ab',60),'hex'),'AES_256_GCM_RANDOM_NONCE_V1',
 1,pg_temp.rehearsal_id('avk_',n),1 FROM generate_series(1,$1::bigint) n;
BEGIN;
INSERT INTO avatar_style_packs(account_id,realm_id,id,current_version)
 SELECT pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),'synthetic',1 FROM generate_series(1,$1::bigint) n;
INSERT INTO avatar_style_pack_versions
 (account_id,realm_id,style_pack_id,version,name,description,style_spec,created_by_kind)
 SELECT pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),'synthetic',1,
 'Synthetic','Synthetic rehearsal style','{}'::jsonb,'system' FROM generate_series(1,$1::bigint) n;
COMMIT;
INSERT INTO realm_avatar_styles(account_id,realm_id,style_pack_id,style_pack_version)
 SELECT pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),'synthetic',1 FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_avatar_profiles(account_id,realm_id,agent_id,style_pack_id,fallback_seed,status)
 SELECT pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),pg_temp.rehearsal_id('agent_',n),
 'synthetic',pg_temp.rehearsal_id('agent_',n),'rejected' FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_avatar_versions
 (account_id,realm_id,agent_id,id,version,style_pack_id,style_pack_version,subject_form,
 svg,description,visual_spec,svg_sha256,proposed_by_kind,proposed_by_id,locked_layers_sha256)
 SELECT pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),pg_temp.rehearsal_id('agent_',n),
 pg_temp.rehearsal_id('avver_',n),1,'synthetic',1,'symbolic',
 '<svg xmlns="http://www.w3.org/2000/svg"><circle r="1"/></svg>','Synthetic','{}'::jsonb,
 repeat('a',64),'agent',pg_temp.rehearsal_id('agent_',n),repeat('b',64) FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_avatar_rejections(id,account_id,realm_id,agent_id,avatar_version,rejected_by_kind,rejected_by_id)
 SELECT pg_temp.rehearsal_id('avrej_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n),
 pg_temp.rehearsal_id('agent_',n),1,'agent',pg_temp.rehearsal_id('agent_',n) FROM generate_series(1,$1::bigint) n;`

const syntheticEmailSQL = `
INSERT INTO realms(id,account_id,name)
 SELECT pg_temp.rehearsal_id('realm_',n+$1::bigint),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),'synthetic_email_'||n
 FROM generate_series(1,$1::bigint) n;
INSERT INTO agents(id,realm_id,name)
 SELECT pg_temp.rehearsal_id('agent_',n+$1::bigint),pg_temp.rehearsal_id('realm_',n+$1::bigint),'synthetic'
 FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_email_addresses
 (id,account_id,realm_id,provisioned_agent_id,domain,agent_segment,realm_label,local_part,provisioning_kind)
 SELECT pg_temp.rehearsal_id('eaddr_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n+$1::bigint),
 pg_temp.rehearsal_id('agent_',n+$1::bigint),'witmail.net','synthetic',pg_temp.rehearsal_id('',n+$1::bigint),
 'synthetic.'||pg_temp.rehearsal_id('',n+$1::bigint),'derived' FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_email_address_domains
 (account_id,realm_id,provisioned_agent_id,address_id,domain,local_part)
 SELECT pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n+$1::bigint),pg_temp.rehearsal_id('agent_',n+$1::bigint),
 pg_temp.rehearsal_id('eaddr_',n),'witmail.net','synthetic.'||pg_temp.rehearsal_id('',n+$1::bigint)
 FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_email_mailboxes(id,account_id,realm_id,owner_agent_id,address_id,receive_state)
 SELECT pg_temp.rehearsal_id('emb_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n+$1::bigint),
 pg_temp.rehearsal_id('agent_',n+$1::bigint),pg_temp.rehearsal_id('eaddr_',n),'enabled' FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_email_realm_aliases
 (claim_id,account_id,realm_id,domain,realm_label,state,controller_revision)
 SELECT pg_temp.rehearsal_id('era_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n+$1::bigint),
 'witmail.net','synthetic-'||n,'applied',1 FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_email_custom_domain_routes
 (domain_request_id,realm_alias_claim_id,account_id,realm_id,domain,realm_label,
 domain_allocation_revision,domain_state_revision,realm_alias_revision,state,controller_revision)
 SELECT pg_temp.rehearsal_id('aedr_',n),pg_temp.rehearsal_id('era_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),
 pg_temp.rehearsal_id('realm_',n+$1::bigint),'example.invalid','synthetic-'||n,1,1,1,'applied',1
 FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_email_messages
 (id,account_id,realm_id,mailbox_id,owner_agent_id,address_id,provider,envelope_sender,
 envelope_recipient,agent_segment,realm_label,raw_mime,raw_size_bytes,raw_sha256,
 duplicate_group_sha256,received_at,attachment_storage_accounted,parse_state)
 SELECT pg_temp.rehearsal_id('emsg_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n+$1::bigint),
 pg_temp.rehearsal_id('emb_',n),pg_temp.rehearsal_id('agent_',n+$1::bigint),pg_temp.rehearsal_id('eaddr_',n),
 'synthetic','synthetic@example.invalid','synthetic.'||pg_temp.rehearsal_id('',n+$1::bigint)||'@witmail.net',
 'synthetic',pg_temp.rehearsal_id('',n+$1::bigint),decode('53796e746865746963','hex'),9,repeat('a',64),repeat('b',64),now(),true,'parsed'
 FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_email_deliveries(message_id,account_id,realm_id,mailbox_id,owner_agent_id)
 SELECT pg_temp.rehearsal_id('emsg_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n+$1::bigint),
 pg_temp.rehearsal_id('emb_',n),pg_temp.rehearsal_id('agent_',n+$1::bigint) FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_email_retry_canary_arms
 (account_id,realm_id,mailbox_id,owner_agent_id,challenge_sha256,expires_at)
 SELECT pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n+$1::bigint),pg_temp.rehearsal_id('emb_',n),
 pg_temp.rehearsal_id('agent_',n+$1::bigint),repeat('a',64),now()+interval '1 hour' FROM generate_series(1,$1::bigint) n;
INSERT INTO agent_email_outbound_messages
 (id,account_id,realm_id,owner_agent_id,address_id,from_address,reply_to_address,to_address,
 body_text,request_kind,thread_key,idempotency_key_hash,request_hash,next_attempt_at)
 SELECT pg_temp.rehearsal_id('esnd_',n),pg_temp.rehearsal_id('acc_',((n-1)%3+1)),pg_temp.rehearsal_id('realm_',n+$1::bigint),
 pg_temp.rehearsal_id('agent_',n+$1::bigint),pg_temp.rehearsal_id('eaddr_',n),
 'synthetic.'||pg_temp.rehearsal_id('',n+$1::bigint)||'@send.witmail.net',
 'synthetic.'||pg_temp.rehearsal_id('',n+$1::bigint)||'@witmail.net','synthetic@example.invalid',
 'Synthetic rehearsal data','direct','synthetic',repeat('a',64),repeat('b',64),now()
 FROM generate_series(1,$1::bigint) n;`

func measureSynthetic(ctx context.Context, db *sql.DB, _ seedOptions) (seedMeasurements, error) {
	var result seedMeasurements
	err := db.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM transcript_entries),
 (SELECT count(*) FROM transcript_conversations),
 (SELECT count(*) FROM memory_curation_mutations),
 (SELECT count(*) FROM memory_curation_run_inputs),
 (SELECT count(DISTINCT request_id) FROM memory_curation_mutations),
 pg_relation_size('transcript_entries'),pg_total_relation_size('transcript_entries'),
 coalesce(sum(pg_column_size(body)::bigint+pg_column_size(payload)),0),
 coalesce(sum(pg_column_size(body)::bigint+coalesce(pg_column_size(payload),0)),0),
 count(payload),count(reply_to_entry_id),coalesce(min(octet_length(body)),0),
 coalesce(percentile_disc(0.5) WITHIN GROUP (ORDER BY octet_length(body)),0),
 coalesce(max(octet_length(body)),0) FROM transcript_entries`).Scan(
		&result.Entries, &result.Transcripts, &result.Mutations, &result.Inputs, &result.Requests,
		&result.HeapBytes, &result.TotalBytes, &result.ColumnBytes, &result.AllColumnBytes,
		&result.PayloadRows, &result.ReplyRows, &result.BodyMinBytes, &result.BodyMedianBytes, &result.BodyMaxBytes)
	if err != nil {
		return seedMeasurements{}, errors.New("measure rehearsal shape")
	}
	return result, nil
}
