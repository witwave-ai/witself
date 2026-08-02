// Shared control-plane view of the provision-token cell contract for
// canonical Realm-ID email routing. Keep paths here so the independent Go
// implementation and every caller have one small alignment surface.

export const CELL_REALM_ROUTE_PAGE_LIMIT = 25;
export const CELL_REALM_ROUTE_LIST_ACTION = "email-realm-routes";
export const CELL_REALM_ROUTE_GET_ACTION = "email-realm-route";
export const CELL_REALM_ROUTE_PREPARE_ACTION =
  "prepare-email-realm-route-retirement";
export const CELL_REALM_ROUTE_COMMIT_ACTION =
  "commit-email-realm-route-retirement";

function accountActionURL(endpoint, accountID, action) {
  return `${endpoint}/v1/accounts/${encodeURIComponent(accountID)}:${action}`;
}

export function cellRealmRouteListURL(
  endpoint,
  accountID,
  cursor = null,
  limit = CELL_REALM_ROUTE_PAGE_LIMIT,
) {
  const url = new URL(accountActionURL(
    endpoint,
    accountID,
    CELL_REALM_ROUTE_LIST_ACTION,
  ));
  url.searchParams.set("limit", String(limit));
  if (cursor) url.searchParams.set("cursor", cursor);
  return url.toString();
}

export function cellRealmRouteGetURL(endpoint, accountID, realmID) {
  const url = new URL(accountActionURL(
    endpoint,
    accountID,
    CELL_REALM_ROUTE_GET_ACTION,
  ));
  url.searchParams.set("realm_id", realmID);
  return url.toString();
}

export function cellRealmRoutePrepareURL(endpoint, accountID) {
  return accountActionURL(endpoint, accountID, CELL_REALM_ROUTE_PREPARE_ACTION);
}

export function cellRealmRouteCommitURL(endpoint, accountID) {
  return accountActionURL(endpoint, accountID, CELL_REALM_ROUTE_COMMIT_ACTION);
}
