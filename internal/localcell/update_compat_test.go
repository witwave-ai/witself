package localcell

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// These bytes were captured by executing HEAD 518fd7e7's writeAtomic and
// journalCause, using a Go overlay with the candidate files replaced by HEAD.
// The corpus covers all 35 accepted legacy operation/step pairs, each with
// started/done/failed and omitted/present causes. Only the live writer's sampled
// timestamps are substituted during comparison; no other bytes are normalized.
const updateHEADManifestFixture = `{
  "layout_version": 1,
  "installation_id": "lc_test",
  "created_at": "2026-01-02T03:04:05.006Z",
  "postgresql_version": "16.test",
  "server_version": "head-server",
  "worker_version": "head-worker",
  "ports": {
    "postgres": 21101,
    "server_api": 21102,
    "server_health": 21103,
    "server_metrics": 21104,
    "worker_health": 21105,
    "worker_metrics": 21106
  },
  "database": "witself",
  "role": "localcell",
  "last_verified_schema_version": 3,
  "last_operation": "install",
  "installed": true
}
`

const updateHEADJournalFixture = `{"operation":"install","step":"operation","state":"started","at":"2026-11-10T20:33:13.153569286Z"}
{"operation":"install","step":"operation","state":"started","at":"2026-11-10T20:33:13.15952308Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"operation","state":"done","at":"2026-11-10T20:33:13.165471244Z"}
{"operation":"install","step":"operation","state":"done","at":"2026-11-10T20:33:13.171449549Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"operation","state":"failed","at":"2026-11-10T20:33:13.177232677Z"}
{"operation":"install","step":"operation","state":"failed","at":"2026-11-10T20:33:13.18311481Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"revert_stop","state":"started","at":"2026-11-10T20:33:13.189564643Z"}
{"operation":"install","step":"revert_stop","state":"started","at":"2026-11-10T20:33:13.196138464Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"revert_stop","state":"done","at":"2026-11-10T20:33:13.203022329Z"}
{"operation":"install","step":"revert_stop","state":"done","at":"2026-11-10T20:33:13.20984393Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"revert_stop","state":"failed","at":"2026-11-10T20:33:13.216583279Z"}
{"operation":"install","step":"revert_stop","state":"failed","at":"2026-11-10T20:33:13.223412981Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"manifest","state":"started","at":"2026-11-10T20:33:13.230030374Z"}
{"operation":"install","step":"manifest","state":"started","at":"2026-11-10T20:33:13.236309512Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"manifest","state":"done","at":"2026-11-10T20:33:13.242753469Z"}
{"operation":"install","step":"manifest","state":"done","at":"2026-11-10T20:33:13.249705516Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"manifest","state":"failed","at":"2026-11-10T20:33:13.25636961Z"}
{"operation":"install","step":"manifest","state":"failed","at":"2026-11-10T20:33:13.262925227Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"password","state":"started","at":"2026-11-10T20:33:13.269123721Z"}
{"operation":"install","step":"password","state":"started","at":"2026-11-10T20:33:13.275526217Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"password","state":"done","at":"2026-11-10T20:33:13.282149401Z"}
{"operation":"install","step":"password","state":"done","at":"2026-11-10T20:33:13.289037187Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"password","state":"failed","at":"2026-11-10T20:33:13.295670645Z"}
{"operation":"install","step":"password","state":"failed","at":"2026-11-10T20:33:13.30279459Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"initdb","state":"started","at":"2026-11-10T20:33:13.309825883Z"}
{"operation":"install","step":"initdb","state":"started","at":"2026-11-10T20:33:13.316752226Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"initdb","state":"done","at":"2026-11-10T20:33:13.323709537Z"}
{"operation":"install","step":"initdb","state":"done","at":"2026-11-10T20:33:13.330482052Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"initdb","state":"failed","at":"2026-11-10T20:33:13.337296131Z"}
{"operation":"install","step":"initdb","state":"failed","at":"2026-11-10T20:33:13.343618665Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"configure","state":"started","at":"2026-11-10T20:33:13.350066186Z"}
{"operation":"install","step":"configure","state":"started","at":"2026-11-10T20:33:13.356368123Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"configure","state":"done","at":"2026-11-10T20:33:13.363131485Z"}
{"operation":"install","step":"configure","state":"done","at":"2026-11-10T20:33:13.369950238Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"configure","state":"failed","at":"2026-11-10T20:33:13.37622659Z"}
{"operation":"install","step":"configure","state":"failed","at":"2026-11-10T20:33:13.38223411Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"postgres","state":"started","at":"2026-11-10T20:33:13.388730289Z"}
{"operation":"install","step":"postgres","state":"started","at":"2026-11-10T20:33:13.395254634Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"postgres","state":"done","at":"2026-11-10T20:33:13.402169002Z"}
{"operation":"install","step":"postgres","state":"done","at":"2026-11-10T20:33:13.408837258Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"postgres","state":"failed","at":"2026-11-10T20:33:13.415630461Z"}
{"operation":"install","step":"postgres","state":"failed","at":"2026-11-10T20:33:13.422214521Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"database","state":"started","at":"2026-11-10T20:33:13.42883363Z"}
{"operation":"install","step":"database","state":"started","at":"2026-11-10T20:33:13.435064529Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"database","state":"done","at":"2026-11-10T20:33:13.441137789Z"}
{"operation":"install","step":"database","state":"done","at":"2026-11-10T20:33:13.447227507Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"database","state":"failed","at":"2026-11-10T20:33:13.453421675Z"}
{"operation":"install","step":"database","state":"failed","at":"2026-11-10T20:33:13.459409889Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"stop","state":"started","at":"2026-11-10T20:33:13.46558573Z"}
{"operation":"install","step":"stop","state":"started","at":"2026-11-10T20:33:13.471199414Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"stop","state":"done","at":"2026-11-10T20:33:13.476613601Z"}
{"operation":"install","step":"stop","state":"done","at":"2026-11-10T20:33:13.482078988Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"stop","state":"failed","at":"2026-11-10T20:33:13.487515254Z"}
{"operation":"install","step":"stop","state":"failed","at":"2026-11-10T20:33:13.49340452Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"principal_credentials","state":"started","at":"2026-11-10T20:33:13.499231713Z"}
{"operation":"install","step":"principal_credentials","state":"started","at":"2026-11-10T20:33:13.505756028Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"principal_credentials","state":"done","at":"2026-11-10T20:33:13.512266145Z"}
{"operation":"install","step":"principal_credentials","state":"done","at":"2026-11-10T20:33:13.518310807Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"principal_credentials","state":"failed","at":"2026-11-10T20:33:13.524790223Z"}
{"operation":"install","step":"principal_credentials","state":"failed","at":"2026-11-10T20:33:13.531449684Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"principals","state":"started","at":"2026-11-10T20:33:13.537814497Z"}
{"operation":"install","step":"principals","state":"started","at":"2026-11-10T20:33:13.544076928Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"principals","state":"done","at":"2026-11-10T20:33:13.550195614Z"}
{"operation":"install","step":"principals","state":"done","at":"2026-11-10T20:33:13.556555594Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"principals","state":"failed","at":"2026-11-10T20:33:13.563658344Z"}
{"operation":"install","step":"principals","state":"failed","at":"2026-11-10T20:33:13.569896633Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"principal_migrations","state":"started","at":"2026-11-10T20:33:13.576058004Z"}
{"operation":"install","step":"principal_migrations","state":"started","at":"2026-11-10T20:33:13.582094076Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"principal_migrations","state":"done","at":"2026-11-10T20:33:13.588180341Z"}
{"operation":"install","step":"principal_migrations","state":"done","at":"2026-11-10T20:33:13.594508345Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"principal_migrations","state":"failed","at":"2026-11-10T20:33:13.601313507Z"}
{"operation":"install","step":"principal_migrations","state":"failed","at":"2026-11-10T20:33:13.608290667Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"principal_grants","state":"started","at":"2026-11-10T20:33:13.615009739Z"}
{"operation":"install","step":"principal_grants","state":"started","at":"2026-11-10T20:33:13.621733355Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"principal_grants","state":"done","at":"2026-11-10T20:33:13.628235203Z"}
{"operation":"install","step":"principal_grants","state":"done","at":"2026-11-10T20:33:13.63470794Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"principal_grants","state":"failed","at":"2026-11-10T20:33:13.641062376Z"}
{"operation":"install","step":"principal_grants","state":"failed","at":"2026-11-10T20:33:13.647771316Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"principal_verified","state":"started","at":"2026-11-10T20:33:13.654234928Z"}
{"operation":"install","step":"principal_verified","state":"started","at":"2026-11-10T20:33:13.6606076Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"principal_verified","state":"done","at":"2026-11-10T20:33:13.666877922Z"}
{"operation":"install","step":"principal_verified","state":"done","at":"2026-11-10T20:33:13.673103669Z","cause":"local_principal_repair_required"}
{"operation":"install","step":"principal_verified","state":"failed","at":"2026-11-10T20:33:13.679319985Z"}
{"operation":"install","step":"principal_verified","state":"failed","at":"2026-11-10T20:33:13.685502029Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"operation","state":"started","at":"2026-11-10T20:33:13.691934107Z"}
{"operation":"start","step":"operation","state":"started","at":"2026-11-10T20:33:13.698651689Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"operation","state":"done","at":"2026-11-10T20:33:13.705104652Z"}
{"operation":"start","step":"operation","state":"done","at":"2026-11-10T20:33:13.711554964Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"operation","state":"failed","at":"2026-11-10T20:33:13.717832553Z"}
{"operation":"start","step":"operation","state":"failed","at":"2026-11-10T20:33:13.724728417Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"revert_stop","state":"started","at":"2026-11-10T20:33:13.731161039Z"}
{"operation":"start","step":"revert_stop","state":"started","at":"2026-11-10T20:33:13.73725984Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"revert_stop","state":"done","at":"2026-11-10T20:33:13.743213241Z"}
{"operation":"start","step":"revert_stop","state":"done","at":"2026-11-10T20:33:13.749905703Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"revert_stop","state":"failed","at":"2026-11-10T20:33:13.756238114Z"}
{"operation":"start","step":"revert_stop","state":"failed","at":"2026-11-10T20:33:13.762305401Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"ports","state":"started","at":"2026-11-10T20:33:13.768421855Z"}
{"operation":"start","step":"ports","state":"started","at":"2026-11-10T20:33:13.774783163Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"ports","state":"done","at":"2026-11-10T20:33:13.781319077Z"}
{"operation":"start","step":"ports","state":"done","at":"2026-11-10T20:33:13.787549464Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"ports","state":"failed","at":"2026-11-10T20:33:13.794275732Z"}
{"operation":"start","step":"ports","state":"failed","at":"2026-11-10T20:33:13.800838892Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"postgres","state":"started","at":"2026-11-10T20:33:13.807712237Z"}
{"operation":"start","step":"postgres","state":"started","at":"2026-11-10T20:33:13.8144536Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"postgres","state":"done","at":"2026-11-10T20:33:13.820624601Z"}
{"operation":"start","step":"postgres","state":"done","at":"2026-11-10T20:33:13.826922863Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"postgres","state":"failed","at":"2026-11-10T20:33:13.833828496Z"}
{"operation":"start","step":"postgres","state":"failed","at":"2026-11-10T20:33:13.840043659Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"server","state":"started","at":"2026-11-10T20:33:13.846292697Z"}
{"operation":"start","step":"server","state":"started","at":"2026-11-10T20:33:13.85254995Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"server","state":"done","at":"2026-11-10T20:33:13.859368314Z"}
{"operation":"start","step":"server","state":"done","at":"2026-11-10T20:33:13.865734603Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"server","state":"failed","at":"2026-11-10T20:33:13.871948259Z"}
{"operation":"start","step":"server","state":"failed","at":"2026-11-10T20:33:13.878198269Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"server_ready","state":"started","at":"2026-11-10T20:33:13.884835348Z"}
{"operation":"start","step":"server_ready","state":"started","at":"2026-11-10T20:33:13.891269954Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"server_ready","state":"done","at":"2026-11-10T20:33:13.897731328Z"}
{"operation":"start","step":"server_ready","state":"done","at":"2026-11-10T20:33:13.904444367Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"server_ready","state":"failed","at":"2026-11-10T20:33:13.911420835Z"}
{"operation":"start","step":"server_ready","state":"failed","at":"2026-11-10T20:33:13.918096304Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"account","state":"started","at":"2026-11-10T20:33:13.924540629Z"}
{"operation":"start","step":"account","state":"started","at":"2026-11-10T20:33:13.931297129Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"account","state":"done","at":"2026-11-10T20:33:13.93825357Z"}
{"operation":"start","step":"account","state":"done","at":"2026-11-10T20:33:13.94498995Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"account","state":"failed","at":"2026-11-10T20:33:13.952074156Z"}
{"operation":"start","step":"account","state":"failed","at":"2026-11-10T20:33:13.958510668Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"worker","state":"started","at":"2026-11-10T20:33:13.96564297Z"}
{"operation":"start","step":"worker","state":"started","at":"2026-11-10T20:33:13.972183023Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"worker","state":"done","at":"2026-11-10T20:33:13.978615343Z"}
{"operation":"start","step":"worker","state":"done","at":"2026-11-10T20:33:13.985271391Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"worker","state":"failed","at":"2026-11-10T20:33:13.992174265Z"}
{"operation":"start","step":"worker","state":"failed","at":"2026-11-10T20:33:13.998807062Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"worker_ready","state":"started","at":"2026-11-10T20:33:14.005247582Z"}
{"operation":"start","step":"worker_ready","state":"started","at":"2026-11-10T20:33:14.012053444Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"worker_ready","state":"done","at":"2026-11-10T20:33:14.018956572Z"}
{"operation":"start","step":"worker_ready","state":"done","at":"2026-11-10T20:33:14.026039322Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"worker_ready","state":"failed","at":"2026-11-10T20:33:14.032255797Z"}
{"operation":"start","step":"worker_ready","state":"failed","at":"2026-11-10T20:33:14.03780078Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"principal_verified","state":"started","at":"2026-11-10T20:33:14.043700577Z"}
{"operation":"start","step":"principal_verified","state":"started","at":"2026-11-10T20:33:14.04932392Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"principal_verified","state":"done","at":"2026-11-10T20:33:14.055041337Z"}
{"operation":"start","step":"principal_verified","state":"done","at":"2026-11-10T20:33:14.060774313Z","cause":"local_principal_repair_required"}
{"operation":"start","step":"principal_verified","state":"failed","at":"2026-11-10T20:33:14.067119306Z"}
{"operation":"start","step":"principal_verified","state":"failed","at":"2026-11-10T20:33:14.072992572Z","cause":"local_principal_repair_required"}
{"operation":"stop","step":"operation","state":"started","at":"2026-11-10T20:33:14.078601743Z"}
{"operation":"stop","step":"operation","state":"started","at":"2026-11-10T20:33:14.084936353Z","cause":"local_principal_repair_required"}
{"operation":"stop","step":"operation","state":"done","at":"2026-11-10T20:33:14.091003519Z"}
{"operation":"stop","step":"operation","state":"done","at":"2026-11-10T20:33:14.097195128Z","cause":"local_principal_repair_required"}
{"operation":"stop","step":"operation","state":"failed","at":"2026-11-10T20:33:14.103127933Z"}
{"operation":"stop","step":"operation","state":"failed","at":"2026-11-10T20:33:14.109106507Z","cause":"local_principal_repair_required"}
{"operation":"stop","step":"revert_stop","state":"started","at":"2026-11-10T20:33:14.115422952Z"}
{"operation":"stop","step":"revert_stop","state":"started","at":"2026-11-10T20:33:14.121812716Z","cause":"local_principal_repair_required"}
{"operation":"stop","step":"revert_stop","state":"done","at":"2026-11-10T20:33:14.127900381Z"}
{"operation":"stop","step":"revert_stop","state":"done","at":"2026-11-10T20:33:14.134123423Z","cause":"local_principal_repair_required"}
{"operation":"stop","step":"revert_stop","state":"failed","at":"2026-11-10T20:33:14.140377446Z"}
{"operation":"stop","step":"revert_stop","state":"failed","at":"2026-11-10T20:33:14.146672319Z","cause":"local_principal_repair_required"}
{"operation":"stop","step":"stop","state":"started","at":"2026-11-10T20:33:14.152820967Z"}
{"operation":"stop","step":"stop","state":"started","at":"2026-11-10T20:33:14.158937302Z","cause":"local_principal_repair_required"}
{"operation":"stop","step":"stop","state":"done","at":"2026-11-10T20:33:14.165428091Z"}
{"operation":"stop","step":"stop","state":"done","at":"2026-11-10T20:33:14.171531866Z","cause":"local_principal_repair_required"}
{"operation":"stop","step":"stop","state":"failed","at":"2026-11-10T20:33:14.177898227Z"}
{"operation":"stop","step":"stop","state":"failed","at":"2026-11-10T20:33:14.184124012Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"operation","state":"started","at":"2026-11-10T20:33:14.190409859Z"}
{"operation":"remove","step":"operation","state":"started","at":"2026-11-10T20:33:14.19662763Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"operation","state":"done","at":"2026-11-10T20:33:14.202912188Z"}
{"operation":"remove","step":"operation","state":"done","at":"2026-11-10T20:33:14.20921733Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"operation","state":"failed","at":"2026-11-10T20:33:14.215534828Z"}
{"operation":"remove","step":"operation","state":"failed","at":"2026-11-10T20:33:14.221739536Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"revert_stop","state":"started","at":"2026-11-10T20:33:14.227955513Z"}
{"operation":"remove","step":"revert_stop","state":"started","at":"2026-11-10T20:33:14.23418865Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"revert_stop","state":"done","at":"2026-11-10T20:33:14.240434852Z"}
{"operation":"remove","step":"revert_stop","state":"done","at":"2026-11-10T20:33:14.246642526Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"revert_stop","state":"failed","at":"2026-11-10T20:33:14.252815897Z"}
{"operation":"remove","step":"revert_stop","state":"failed","at":"2026-11-10T20:33:14.259137267Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"stop","state":"started","at":"2026-11-10T20:33:14.265477246Z"}
{"operation":"remove","step":"stop","state":"started","at":"2026-11-10T20:33:14.27154031Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"stop","state":"done","at":"2026-11-10T20:33:14.277618373Z"}
{"operation":"remove","step":"stop","state":"done","at":"2026-11-10T20:33:14.284071151Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"stop","state":"failed","at":"2026-11-10T20:33:14.290245608Z"}
{"operation":"remove","step":"stop","state":"failed","at":"2026-11-10T20:33:14.296542408Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"spool","state":"started","at":"2026-11-10T20:33:14.302754308Z"}
{"operation":"remove","step":"spool","state":"started","at":"2026-11-10T20:33:14.308722901Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"spool","state":"done","at":"2026-11-10T20:33:14.315633346Z"}
{"operation":"remove","step":"spool","state":"done","at":"2026-11-10T20:33:14.321810442Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"spool","state":"failed","at":"2026-11-10T20:33:14.328131526Z"}
{"operation":"remove","step":"spool","state":"failed","at":"2026-11-10T20:33:14.334738488Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"tmp","state":"started","at":"2026-11-10T20:33:14.341927124Z"}
{"operation":"remove","step":"tmp","state":"started","at":"2026-11-10T20:33:14.348161763Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"tmp","state":"done","at":"2026-11-10T20:33:14.354434203Z"}
{"operation":"remove","step":"tmp","state":"done","at":"2026-11-10T20:33:14.360411419Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"tmp","state":"failed","at":"2026-11-10T20:33:14.367007607Z"}
{"operation":"remove","step":"tmp","state":"failed","at":"2026-11-10T20:33:14.373069609Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"logs","state":"started","at":"2026-11-10T20:33:14.378795426Z"}
{"operation":"remove","step":"logs","state":"started","at":"2026-11-10T20:33:14.384572805Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"logs","state":"done","at":"2026-11-10T20:33:14.390001434Z"}
{"operation":"remove","step":"logs","state":"done","at":"2026-11-10T20:33:14.395672534Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"logs","state":"failed","at":"2026-11-10T20:33:14.401135041Z"}
{"operation":"remove","step":"logs","state":"failed","at":"2026-11-10T20:33:14.406958116Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"purge","state":"started","at":"2026-11-10T20:33:14.412773043Z"}
{"operation":"remove","step":"purge","state":"started","at":"2026-11-10T20:33:14.418504019Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"purge","state":"done","at":"2026-11-10T20:33:14.424164554Z"}
{"operation":"remove","step":"purge","state":"done","at":"2026-11-10T20:33:14.429905229Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"purge","state":"failed","at":"2026-11-10T20:33:14.435691476Z"}
{"operation":"remove","step":"purge","state":"failed","at":"2026-11-10T20:33:14.441923869Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"runtime_cleanup","state":"started","at":"2026-11-10T20:33:14.448006034Z"}
{"operation":"remove","step":"runtime_cleanup","state":"started","at":"2026-11-10T20:33:14.454162336Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"runtime_cleanup","state":"done","at":"2026-11-10T20:33:14.460774652Z"}
{"operation":"remove","step":"runtime_cleanup","state":"done","at":"2026-11-10T20:33:14.467139212Z","cause":"local_principal_repair_required"}
{"operation":"remove","step":"runtime_cleanup","state":"failed","at":"2026-11-10T20:33:14.473204612Z"}
{"operation":"remove","step":"runtime_cleanup","state":"failed","at":"2026-11-10T20:33:14.479401704Z","cause":"local_principal_repair_required"}
`

// These frozen wire structs are copied from HEAD 518fd7e7. The same HEAD overlay
// executed readManifest/readJournal probes: generation fields are unknown record
// fields; update_id and the update operation are refused at journal byte zero.
type updateHEADManifest struct {
	LayoutVersion             int                     `json:"layout_version"`
	InstallationID            string                  `json:"installation_id"`
	CreatedAt                 time.Time               `json:"created_at"`
	PostgreSQLVersion         string                  `json:"postgresql_version"`
	ServerVersion             string                  `json:"server_version"`
	WorkerVersion             string                  `json:"worker_version"`
	Ports                     Ports                   `json:"ports"`
	Database                  string                  `json:"database"`
	Role                      string                  `json:"role"`
	PrincipalLayoutVersion    int                     `json:"principal_layout_version,omitempty"`
	PrincipalRoles            []string                `json:"principal_roles,omitempty"`
	AccountID                 string                  `json:"account_id,omitempty"`
	LastVerifiedSchemaVersion int                     `json:"last_verified_schema_version"`
	LastOperation             string                  `json:"last_operation"`
	Installed                 bool                    `json:"installed"`
	Cleanup                   *updateHEADJournalEntry `json:"cleanup,omitempty"`
	MigrationWait             time.Duration           `json:"migration_wait,omitempty"`
	JobRestartPolicy          string                  `json:"job_restart_policy,omitempty"`
}

type updateHEADJournalEntry struct {
	Operation string    `json:"operation"`
	Step      string    `json:"step"`
	State     string    `json:"state"`
	At        time.Time `json:"at"`
	Cause     string    `json:"cause,omitempty"`
}

func TestUpdateHEADManifestByteIdentity(t *testing.T) {
	c, _ := unitCell(t)
	if err := testWriteFile(c.path("manifest.json"), []byte(updateHEADManifestFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := readManifest(c.cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(c.path("manifest.json"), m, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.path("manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != updateHEADManifestFixture {
		t.Fatal("HEAD manifest changed bytes without generation fields")
	}
}

func TestUpdateHEADJournalLineShapes(t *testing.T) {
	c, _ := unitCell(t)
	lines := bytes.Split(bytes.TrimSuffix([]byte(updateHEADJournalFixture), []byte("\n")), []byte("\n"))
	if len(lines) != 210 {
		t.Fatal("HEAD journal corpus is incomplete")
	}
	for _, line := range lines {
		var entry journalEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		if !validJournalStep(entry.Operation, entry.Step) {
			t.Fatal("legacy journal vocabulary changed")
		}
		if err := c.journalCause(entry.Operation, entry.Step, entry.State, entry.Cause); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(c.path("journal", "operations.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	gotLines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	if len(gotLines) != len(lines) {
		t.Fatal("legacy journal line count changed")
	}
	for i, line := range lines {
		var got, want map[string]json.RawMessage
		if err := json.Unmarshal(line, &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(gotLines[i], &got); err != nil {
			t.Fatal(err)
		}
		gotTime, ok := got["at"]
		if !ok {
			t.Fatal("legacy journal timestamp missing")
		}
		var sampled time.Time
		if err := json.Unmarshal(gotTime, &sampled); err != nil || sampled.IsZero() {
			t.Fatal("legacy journal timestamp is invalid")
		}
		normalized := bytes.Replace(gotLines[i], append([]byte(`"at":`), gotTime...), append([]byte(`"at":`), want["at"]...), 1)
		if !bytes.Equal(normalized, line) {
			t.Fatalf("HEAD journal wire shape changed at corpus line %d", i+1)
		}
	}
}

func TestUpdateLegacyJournalReaderParity(t *testing.T) {
	type fixture struct {
		name, data string
		absent     bool
		directory  bool
		cleanup    bool
		nonPrivate bool
		symlink    bool
	}
	cases := []fixture{
		{name: "absent", absent: true},
		{name: "empty"},
		{name: "absent_with_cleanup", absent: true, cleanup: true},
		{name: "empty_with_cleanup", cleanup: true},
	}
	type corpusEntry struct {
		journalEntry
		line string
	}
	var corpus []corpusEntry
	for _, line := range strings.Split(strings.TrimSuffix(updateHEADJournalFixture, "\n"), "\n") {
		var entry journalEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		corpus = append(corpus, corpusEntry{entry, line + "\n"})
	}
	// Each cause variant forms a complete operation. Place operation/done last
	// so all other corpus records (including failed states) are fenced. Together
	// these eight sequences exercise every one of the 210 captured lines.
	used := 0
	for _, operation := range []string{"install", "start", "stop", "remove"} {
		for _, withCause := range []bool{false, true} {
			var start, body, end string
			for _, entry := range corpus {
				if entry.Operation != operation || (entry.Cause != "") != withCause {
					continue
				}
				used++
				switch {
				case entry.Step == "operation" && entry.State == "started":
					start = entry.line
				case entry.Step == "operation" && entry.State == "done":
					end = entry.line
				default:
					body += entry.line
				}
			}
			if start == "" || end == "" {
				t.Fatal("HEAD corpus lacks a complete legacy operation")
			}
			cases = append(cases, fixture{name: fmt.Sprintf("complete_%s_cause_%t", operation, withCause), data: start + body + end})
		}
	}
	if used != 210 || used != len(corpus) {
		t.Fatal("parity sequences did not cover the entire HEAD corpus")
	}
	line := func(operation, step, state string) string {
		t.Helper()
		for _, entry := range corpus {
			if entry.Operation == operation && entry.Step == step && entry.State == state && entry.Cause == "" {
				return entry.line
			}
		}
		t.Fatal("missing HEAD corpus entry")
		return ""
	}
	start := line("start", "operation", "started")
	pending := start + line("start", "ports", "done") + line("start", "server", "failed")
	oversizeNUL := strings.Repeat("\x00", 2*64*1024+7)
	cases = append(cases,
		fixture{name: "open_operation", data: start},
		fixture{name: "pending_done_and_failed_steps", data: pending},
		fixture{name: "identical_duplicate_step_lines", data: start + line("start", "ports", "done") + line("start", "ports", "done")},
		fixture{name: "crlf_line_endings", data: strings.ReplaceAll(pending, "\n", "\r\n")},
		fixture{name: "oversize_line_without_nul", data: pending + strings.Repeat(" ", 2*64*1024+7) + start},
		fixture{name: "non_private_mode", data: pending, nonPrivate: true},
		fixture{name: "symlink", data: pending, symlink: true},
		fixture{name: "json_prefix_tail", data: pending + `{"operation":`},
		fixture{name: "nul_tail", data: pending + strings.Repeat("\x00", 7)},
		fixture{name: "oversize_nul_tail", data: pending + oversizeNUL},
		fixture{name: "nul_then_newline", data: pending + strings.Repeat("\x00", 7) + "\n"},
		fixture{name: "oversize_nul_then_newline", data: pending + oversizeNUL + "\n"},
		fixture{name: "oversize_nul_then_non_nul", data: pending + oversizeNUL + "x"},
		fixture{name: "unknown_member", data: strings.Replace(start, "{", `{"unknown":true,`, 1)},
		fixture{name: "unknown_operation", data: strings.Replace(start, `"operation":"start"`, `"operation":"unknown"`, 1)},
		fixture{name: "unknown_step", data: start + strings.Replace(line("start", "ports", "done"), `"step":"ports"`, `"step":"unknown"`, 1)},
		fixture{name: "invalid_state", data: strings.Replace(start, `"state":"started"`, `"state":"invalid"`, 1)},
		fixture{name: "zero_timestamp", data: strings.Replace(start, corpusTimestamp(t, start), "0001-01-01T00:00:00Z", 1)},
		fixture{name: "trailing_bytes_same_line", data: strings.TrimSuffix(start, "\n") + " {}\n"},
		fixture{name: "step_without_operation", data: line("start", "ports", "done")},
		fixture{name: "overlapping_operations", data: start + line("stop", "operation", "started")},
		fixture{name: "mismatched_operation", data: start + line("stop", "stop", "done")},
		fixture{name: "directory", directory: true},
	)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, m := unitCell(t)
			if tc.cleanup {
				m.Cleanup = &journalEntry{Operation: "remove", Step: "runtime_cleanup", State: "started", At: m.CreatedAt}
				if err := writeAtomic(c.path("manifest.json"), m, nil); err != nil {
					t.Fatal(err)
				}
			}
			path := c.path("journal", "operations.jsonl")
			if tc.directory {
				if err := testMkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if !tc.absent {
				if err := testWriteFile(path, []byte(tc.data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// Windows custody is by ACL and the mode change is a no-op there, so
			// the row still runs natively as a plain parity row instead of skipping.
			if tc.nonPrivate {
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.symlink {
				target := path + ".retained"
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					if runtime.GOOS == "windows" {
						t.Skip("LC-S2-PW3: Windows symlink fixture privileges unavailable")
					}
					t.Fatal(err)
				}
			}
			shipped, shippedErr := c.readJournal()
			private, privateErr := c.readUpdateJournal()
			if !reflect.DeepEqual(shipped.pending, private.pending) || !reflect.DeepEqual(shipped.done, private.done) ||
				shipped.durableBytes != private.durableBytes || shipped.tornTail != private.tornTail ||
				(shippedErr == nil) != (privateErr == nil) || fmt.Sprint(shippedErr) != fmt.Sprint(privateErr) {
				t.Fatalf("journal reader mismatch: shipped pending=%#v done=%#v bytes=%d torn=%t error=%v; private pending=%#v done=%#v bytes=%d torn=%t error=%v",
					shipped.pending, shipped.done, shipped.durableBytes, shipped.tornTail, shippedErr,
					private.pending, private.done, private.durableBytes, private.tornTail, privateErr)
			}
			if strings.HasPrefix(tc.name, "complete_") && (shippedErr != nil || shipped.pending != nil || shipped.done != nil || shipped.tornTail || shipped.durableBytes != int64(len(tc.data))) {
				t.Fatal("HEAD corpus sequence was not accepted as a complete operation")
			}
			if tc.cleanup && (shippedErr != nil || !reflect.DeepEqual(shipped.pending, &Interruption{Operation: "remove", Step: "runtime_cleanup"})) {
				t.Fatal("manifest cleanup did not restore the pending interruption")
			}
			t.Logf("both: pending=%#v; done=%#v; durableBytes=%d; tornTail=%t; error=%v", shipped.pending, shipped.done, shipped.durableBytes, shipped.tornTail, shippedErr)
		})
	}
}

func corpusTimestamp(t *testing.T, line string) string {
	t.Helper()
	var entry map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatal(err)
	}
	var timestamp string
	if err := json.Unmarshal(entry["at"], &timestamp); err != nil {
		t.Fatal(err)
	}
	return timestamp
}

func TestUpdatePrivateJournalContentRefusedByShippedReader(t *testing.T) {
	entry := func(operation, step, state, extra string) string {
		return fmt.Sprintf("{\"operation\":%q,\"step\":%q,\"state\":%q,\"at\":\"2026-01-02T03:04:05Z\"%s}\n", operation, step, state, extra)
	}
	type fixture struct{ name, prefix, line string }
	start := entry("update", "operation", "started", `,"update_id":"update_test"`)
	cases := []fixture{{name: "update_started", line: start}}
	prefix := start
	for _, step := range []string{"prepared", "maintenance_requested", "acknowledged", "quiesced", "recovery_verified", "migrating", "activated", "health_verified", "committed", "revert_stop", "operation"} {
		for _, state := range []string{"started", "done", "failed"} {
			if step == "operation" && state == "started" {
				continue
			}
			cases = append(cases, fixture{name: step + "_" + state, prefix: prefix, line: entry("update", step, state, "")})
		}
		if step != "operation" {
			prefix += entry("update", step, "done", "")
		}
	}
	// Names stay inside the closed subtest-name alphabet of CI's result checker.
	for _, legacy := range []struct{ name, value string }{{"empty_string", `""`}, {"null", "null"}} {
		cases = append(cases, fixture{name: "legacy_update_id_" + legacy.name, line: entry("stop", "operation", "started", `,"update_id":`+legacy.value)})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := unitCell(t)
			path := c.path("journal", "operations.jsonl")
			if err := testWriteFile(path, []byte(tc.prefix+tc.line), 0o600); err != nil {
				t.Fatal(err)
			}
			private, err := c.readUpdateJournal()
			if err != nil || private.tornTail || private.durableBytes != int64(len(tc.prefix+tc.line)) {
				t.Fatalf("private reader refused valid update content: %v", err)
			}
			// Probe this exact line alone: an earlier update/start must not mask
			// accidental acceptance of any subsequent update-bearing record.
			if err := testWriteFile(path, []byte(tc.line), 0o600); err != nil {
				t.Fatal(err)
			}
			shipped, err := c.readJournal()
			if err == nil || err.Error() != "invalid local cell journal; manual recovery required" || shipped.durableBytes != 0 {
				t.Fatalf("shipped reader accepted update content: %v", err)
			}
		})
	}
}

func TestUpdateHEADOptionalFieldsDowngrade(t *testing.T) {
	t.Run("manifest", func(t *testing.T) {
		for _, tc := range []struct{ name, member string }{
			{"active_nonempty", `"active_generation":"generation-test"`},
			{"active_empty", `"active_generation":""`},
			{"active_null", `"active_generation":null`},
			{"maintenance_nonzero", `"maintenance_generation":1`},
			{"maintenance_zero", `"maintenance_generation":0`},
			{"maintenance_null", `"maintenance_generation":null`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				c, _ := unitCell(t)
				// Splice raw bytes: marshaling Manifest would omit precisely the
				// empty/null members whose rejection must survive a downgrade.
				data := strings.Replace(updateHEADManifestFixture, "{", "{\n  "+tc.member+",", 1)
				if err := testWriteFile(c.path("manifest.json"), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := readManifest(c.cfg.Root); err == nil || err.Error() != "invalid local cell record" {
					t.Fatalf("shipped reader did not refuse generation member: %v", err)
				}
				var old updateHEADManifest
				if err := readJSON(c.path("manifest.json"), &old); err == nil || err.Error() != "invalid local cell record" {
					t.Fatal("HEAD wire schema accepted a new generation field")
				}
			})
		}
	})
	t.Run("journal_field", func(t *testing.T) {
		entry := updateJournalEntry{Operation: "start", Step: "operation", State: "started", At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), UpdateID: "operation-test"}
		line, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		var old updateHEADJournalEntry
		if err := decoder.Decode(&old); err == nil {
			t.Fatal("HEAD journal schema accepted update_id")
		}
	})
	t.Run("journal_operation", func(t *testing.T) {
		c, _ := unitCell(t)
		entry := journalEntry{Operation: "update", Step: "operation", State: "started", At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
		line, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		var old updateHEADJournalEntry
		if err := decoder.Decode(&old); err != nil || decoder.Decode(new(any)) != io.EOF {
			t.Fatal("update operation unexpectedly changes legacy field shape")
		}
		if err := testWriteFile(c.path("journal", "operations.jsonl"), append(line, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		progress, err := c.readJournal()
		if err == nil || err.Error() != "invalid local cell journal; manual recovery required" || progress.durableBytes != 0 {
			t.Fatal("ordinary journal reader accepted an update operation")
		}
	})
}

// Empty and null new fields must not loosen HEAD's strict corruption boundary,
// including the journal entry nested in a legacy manifest cleanup marker.
func TestUpdateOrdinaryReadersRejectOptionalJournalField(t *testing.T) {
	for _, value := range []any{"", nil, "operation-test"} {
		c, m := unitCell(t)
		entry := map[string]any{"operation": "stop", "step": "operation", "state": "started", "at": m.CreatedAt, "update_id": value}
		line, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		if err := testWriteFile(c.path("journal", "operations.jsonl"), append(line, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		progress, err := c.readJournal()
		if err == nil || err.Error() != "invalid local cell journal; manual recovery required" || progress.durableBytes != 0 {
			t.Fatal("ordinary journal reader accepted an optional update field")
		}
		manifest, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err := json.Unmarshal(manifest, &wire); err != nil {
			t.Fatal(err)
		}
		wire["cleanup"] = entry
		if err := writeAtomic(c.path("manifest.json"), wire, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := readManifest(c.cfg.Root); err == nil || err.Error() != "invalid local cell record" {
			t.Fatal("legacy manifest cleanup accepted an optional update field")
		}
	}
}
