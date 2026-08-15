const WITSELF_CELL_HOST =
  /^api\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.cells\.witself\.witwave\.ai$/;
const CIVO_CELL_HOST =
  /^api\.[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.k8s\.civo\.com$/;

export function isProductionCellHost(hostname) {
  return WITSELF_CELL_HOST.test(hostname) || CIVO_CELL_HOST.test(hostname);
}
