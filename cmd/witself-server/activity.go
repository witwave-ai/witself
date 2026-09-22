package main

import (
	"context"
	"errors"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

func configureActivity(cfg *server.Config, st *store.Store) {
	cfg.GetActivity = func(ctx context.Context, p server.DomainPrincipal, q server.ActivityQuery) (server.ActivityReport, error) {
		report, err := st.GetActivity(ctx, toStorePrincipal(p), store.ActivityQuery{Since: q.Since, Until: q.Until, Bucket: q.Bucket})
		switch {
		case errors.Is(err, store.ErrUsageForbidden):
			return server.ActivityReport{}, server.ErrForbidden
		case errors.Is(err, store.ErrUsageInputInvalid), errors.Is(err, store.ErrUsageQueryTooLarge):
			return server.ActivityReport{}, server.ErrBadInput
		case err != nil:
			return server.ActivityReport{}, err
		}
		return toServerActivityReport(report), nil
	}
}
func toServerActivityReport(r store.ActivityReport) server.ActivityReport {
	u := toServerUsageReport(store.UsageReport{Points: r.Points, Totals: r.Totals})
	return server.ActivityReport{Schema: r.Schema, Catalog: r.Catalog, AccountID: r.AccountID, RealmID: r.RealmID, AgentID: r.AgentID, Since: r.Since, Until: r.Until, Bucket: r.Bucket, TrackingSince: r.TrackingSince, Points: u.Points, Totals: u.Totals, Truncated: r.Truncated}
}
