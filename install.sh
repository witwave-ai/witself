#!/bin/sh
# install.sh — universal installer for Witself release binaries.
#
#   # witself (default):
#   curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh
#   # witself-infra:
#   curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh -s witself-infra
#   # witself-admin:
#   curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh -s witself-admin
#
# Downloads the selected binary for your OS/arch from the GitHub releases,
# verifies its SHA-256 checksum, and installs it on your PATH.
#
# Usage:
#   sh                 install latest witself
#   sh -s BINARY       install latest witself, witself-infra, witself-server, or witself-admin
#   sh -s BINARY VER   install a specific binary version
#   sh -s VER          install a specific witself version
#
# Environment:
#   WITSELF_BINARY   back-compat binary selector; positional BINARY wins
#   WS_VERSION       version to install (e.g. v0.0.1); default: latest release
#   WS_INSTALL_DIR   install directory; default: /usr/local/bin (sudo if needed)
#   WS_RELEASE_DIR   absolute directory containing an archive and checksums.txt;
#                    requires an explicit version and skips all network access
#   WS_COMPONENT_SET_DIR   explicit signed bundle; unset by default
#   WS_TRUST_ROOT_PEM_FILE explicit TEST-ONLY public root; no production root
#   WS_UNTRUSTED_CLOCK     1 refuses component-set acquisition
#   WS_INSTALL_LOCK_TIMEOUT
#                    seconds to wait for another installer; default: 30
#
# Note: witself-infra drives the `pulumi` engine at runtime. Unlike `brew install`
# (which pulls it automatically), this installer cannot — install pulumi yourself.

# The bootstrap library is deliberately embedded: a fetched installer never fetches
# code to authenticate itself. Only the explicit component-set path calls it.
# Output is a closed reason code; authenticated fields stay in private scratch.
ws_cs_reason() { printf '%s\n' "$1"; return 1; }
ws_cs_size() { [ -f "$1" ] && [ ! -L "$1" ] && [ "$(wc -c <"$1" | tr -d '[:space:]')" -le "$2" ]; }

ws_cs_json() { # input mode output-directory expected-kind
  # BSD awk strings cannot represent raw NUL reliably. JSON forbids raw control
  # bytes, so reject NUL before the strict parser can observe a truncated line.
  [ "$(LC_ALL=C tr -cd '\000' <"$1" | wc -c | tr -d '[:space:]')" -eq 0 ] || { ws_cs_reason invalid_json; return 1; }
  LC_ALL=C awk -v mode="$2" -v out="$3" -v wantkind="$4" \
    -v now="${WS_CS_NOW:-}" -v wanttag="${WS_CS_TAG:-}" \
    -v wantos="${WS_CS_OS:-}" -v wantarch="${WS_CS_ARCH:-}" '
  function bad(r) { print r; failed=1; exit 1 }
  function white() { while (substr(src,pos,1) ~ /[ \t\r\n]/ && pos<=length(src)) pos++ }
  function hex(s, i,c,n) { n=0; for(i=1;i<=length(s);i++){ c=index("0123456789abcdef",tolower(substr(s,i,1)))-1; if(c<0) bad("invalid_json"); n=n*16+c } return n }
  function utf(c) { if(c<128) return sprintf("%c",c); if(c<2048) return sprintf("%c%c",192+int(c/64),128+c%64); if(c<65536) return sprintf("%c%c%c",224+int(c/4096),128+int(c/64)%64,128+c%64); return sprintf("%c%c%c%c",240+int(c/262144),128+int(c/4096)%64,128+int(c/64)%64,128+c%64) }
  function string(   r,c,e,u,l,b,q,j,cp,min) {
    if(substr(src,pos++,1)!="\"") bad("invalid_json"); r=""
    while(pos<=length(src)) {
      c=substr(src,pos++,1)
      if(c=="\"") { if(length(r)>4096) bad("profile_violation"); return r }
      if(c=="\\") {
        e=substr(src,pos++,1)
        if(e=="\"" || e=="\\" || e=="/") c=e
        else if(e=="b") c=sprintf("%c",8)
        else if(e=="f") c=sprintf("%c",12)
        else if(e=="n") c="\n"
        else if(e=="r") c="\r"
        else if(e=="t") c="\t"
        else if(e=="u") {
          if(length(substr(src,pos,4))!=4) bad("invalid_json"); u=hex(substr(src,pos,4)); pos+=4
          if(u>=55296 && u<=56319) {
            if(substr(src,pos,2)!="\\u") bad("profile_violation"); pos+=2
            l=hex(substr(src,pos,4)); pos+=4
            if(l<56320 || l>57343) bad("profile_violation"); u=65536+(u-55296)*1024+l-56320
          } else if(u>=56320 && u<=57343) bad("profile_violation")
          if(u==0)c="\001";else c=utf(u)
        } else bad("invalid_json")
      } else {
        b=byte[c]; if(b<32) bad("invalid_json")
        if(b>=128) {
          q=b>=194&&b<=223?1:b>=224&&b<=239?2:b>=240&&b<=244?3:-1
          if(q<0) bad("profile_violation")
          cp=b-(q==1?192:q==2?224:240); min=q==1?128:q==2?2048:65536
          for(j=0;j<q;j++) { e=substr(src,pos++,1); b=byte[e]; if(b<128||b>191) bad("profile_violation"); cp=cp*64+b-128; c=c e }
          if(cp<min||cp>1114111||(cp>=55296&&cp<=57343)) bad("profile_violation")
        }
      }
      r=r c; if(length(r)>4096) bad("profile_violation")
    }
    bad("invalid_json")
  }
  function parse(depth,   id,c,key,child,start,num) {
    white(); c=substr(src,pos,1); id=++nodes
    if(c=="{"||c=="[") {
      if(depth>8) bad("profile_violation"); type[id]=c; pos++; white()
      if(substr(src,pos,1)==(c=="{"?"}":"]")){pos++;return id}
      while(1) {
        if(c=="{") { white(); key=string(); if((id SUBSEP key) in member) bad("duplicate_key"); white(); if(substr(src,pos++,1)!=":") bad("invalid_json") }
        else key=count[id]+1
        child=parse(depth+1); member[id,key]=child; keys[id,++count[id]]=key
        white(); if(substr(src,pos,1)==(c=="{"?"}":"]")){pos++;return id}
        if(substr(src,pos++,1)!=",") bad("invalid_json")
      }
    }
    if(c=="\""){type[id]="s";val[id]=string();return id}
    start=pos
    while(substr(src,pos,1) !~ /[ \t\r\n,}\]]/ && pos<=length(src))pos++
    num=substr(src,start,pos-start)
    if(num=="true"||num=="false"){type[id]="b";val[id]=num;return id}
    if(num=="null"){type[id]="null";return id}
    if(num !~ /^(0|[1-9][0-9]*)$/) { if(num ~ /^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$/){type[id]="f";val[id]=num;return id};bad("invalid_json") }
    if(length(num)>20||(length(num)==20&&("x"num)>("x18446744073709551615"))) bad("profile_violation")
    type[id]="n";val[id]=num;return id
  }
  function field(id,k,t,  x) { x=member[id,k];if(t=="n"&&type[x]=="f")bad("profile_violation");if(!x||type[x]!=t)bad("invalid_document");return x }
  function str(id,k) { return val[field(id,k,"s")] }
  function number(id,k) { return val[field(id,k,"n")] }
  function positive(id,k, x){x=number(id,k);if(x==0)bad("sequence_invalid");return x}
  function array(id,k) { return field(id,k,"[") }
  function object(id,k) { return field(id,k,"{") }
  function known(s,k,   a,i,n){n=split(s,a," ");for(i=1;i<=n;i++)if(a[i]==k)return 1;return 0}
  function allowed(id,s,top,   i,k) { for(i=1;i<=count[id];i++){k=keys[id,i];if(!known(s,k)&&!top)bad("unknown_field")} }
  function uintcmp(a,b){if(length(a)!=length(b))return length(a)<length(b)?-1:1;return ("x"a)==("x"b)?0:("x"a)<("x"b)?-1:1}
  function b64(s){return s!=""&&s!~/[^A-Za-z0-9+\/=]/}
  function exact(id,s) { if(type[id]!="{")bad("invalid_document");allowed(id,s,0) }
  function h64(s) { return length(s)==64&&s!~/[^0-9a-f]/ }
  function safe(s,   a,i,n,b) {
    if(s==""||length(s)>1024||s~/[\\:\001-\037]/)return 0
    n=split(s,a,"/");for(i=1;i<=n;i++)if(a[i]==""||a[i]=="."||a[i]=="..")return 0
    # Conservative, normalization-stable ASCII and Latin-1 path subset. The
    # Unicode 16 normalization/inventory activation contract remains in Go/B3.
    for(i=1;i<=length(s);i++){b=byte[substr(s,i,1)];if(b>=128){if(b!=194&&b!=195)return 0;i++}}
    return 1
  }
  function path(id,k, s){s=str(id,k);if(!safe(s))bad("path_invalid");return s}
  function timeok(s, y,m,d,h,n,z,days) {
    if(length(s)!=20||s!~/^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z$/)return 0
    y=substr(s,1,4)+0;m=substr(s,6,2)+0;d=substr(s,9,2)+0;h=substr(s,12,2)+0;n=substr(s,15,2)+0;z=substr(s,18,2)+0
    days=m==2?28+(y%4==0&&(y%100!=0||y%400==0)):m==4||m==6||m==9||m==11?30:31
    return m>=1&&m<=12&&d>=1&&d<=days&&h<24&&n<60&&z<60
  }
  function put(name,value){print value > (out "/" name);close(out "/" name)}
  function keyobj(id,prefix,   kid,spki) { exact(id,"key_id spki");kid=str(id,"key_id");spki=str(id,"spki");if(!h64(kid))bad("invalid_key");if(!b64(spki))bad("invalid_encoding");put(prefix".id",kid);put(prefix".spki",spki) }
  function unique_strings(id,reason,paths,   i,x,s,j) {
    for(i=1;i<=count[id];i++){x=member[id,i];if(type[x]!="s")bad("invalid_document");s=val[x];if(paths&&!safe(s))bad("path_invalid");for(j=1;j<i;j++)if(val[member[id,j]]==s)bad(reason)}
  }
  function common(id, fields,   i,c,k) {
    if(number(id,"format_version")!=1)bad("invalid_document")
    if(str(id,"kind")!=wantkind)bad("wrong_kind")
    c=array(id,"critical");unique_strings(c,"invalid_document",0)
    for(i=1;i<=count[c];i++)if(!known(fields,val[member[c,i]]))bad("unknown_critical")
    issued=str(id,"issued_at");expires=str(id,"expires_at")
    if(!timeok(issued)||!timeok(expires)||expires<=issued)bad("invalid_document")
    if(!timeok(now))bad("unknown_clock")
    if(now<issued)bad("not_yet_valid");if(now>=expires)bad("expired")
    put("issued",issued);put("expires",expires);put("root_sequence",positive(id,"root_sequence"))
  }
  function envelope(id,   a,i,x,k) {
    exact(id,"format_version kind algorithm signatures")
    if(number(id,"format_version")!=1)bad("invalid_document")
    if(str(id,"kind")!=wantkind)bad("wrong_kind")
    if(str(id,"algorithm")!="rsa3072-pkcs1v15-sha256")bad("wrong_algorithm")
    a=array(id,"signatures");if(count[a]!=(wantkind=="root_rotation"?2:1))bad("signature_count")
    put("count",count[a])
    for(i=1;i<=count[a];i++){x=member[a,i];exact(x,"key_id signature");k=str(x,"key_id");if(!h64(k))bad("invalid_key");if(seen[k]++)bad("signature_count");put("sig"i".id",k);if(!b64(str(x,"signature")))bad("invalid_encoding");put("sig"i".b64",str(x,"signature"))}
  }
  function component(id,pi,ci,   role,a,inv,e,i,p,sz,total,sc,own,arr,j,part,pg,deps,k) {
    exact(id,"role version archive inventory depends_on required_extensions schema ownership postgresql")
    role=str(id,"role");if(role!~/^(app|service_host|server|worker|postgresql|extension)$/)bad("roles_invalid")
    if(++roles[pi,role]>1&&role!="worker")bad("roles_invalid")
    if(str(id,"version")=="")bad("invalid_document")
    a=object(id,"archive");exact(a,"name size sha256 format");p=path(a,"name")
    if(index(p,"/"))bad("path_invalid");if(archives[pi,p]++)bad("inventory_invalid")
    sz=number(a,"size");if(sz==0||uintcmp(sz,"2147483648")>0)bad("inventory_invalid")
    if(!h64(str(a,"sha256")))bad("inventory_invalid")
    if(str(a,"format")!="tar.gz"&&str(a,"format")!="zip")bad("invalid_document")
    inv=array(id,"inventory");if(count[inv]<1||count[inv]>4096)bad("inventory_invalid");total=0
    for(i=1;i<=count[inv];i++) {
      e=member[inv,i];exact(e,"path size sha256 mode executable");p=path(e,"path")
      if(inventory[pi,p]++)bad("inventory_invalid");owned[id,p]=1;paths[pi,++pathcount[pi]]=p
      if(uintcmp(number(e,"size"),sz)>0)bad("inventory_invalid");total+=number(e,"size");if(total>(sz+0))bad("inventory_invalid")
      if(!h64(str(e,"sha256")))bad("inventory_invalid")
      k=str(e,"mode");if(k!="0644"&&k!="0755")bad("mode_invalid")
      if(val[field(e,"executable","b")]!=(k=="0755"?"true":"false"))bad("mode_invalid")
    }
    sc=object(id,"schema");exact(sc,"target accepted_min accepted_max")
    if(uintcmp(number(sc,"target"),number(sc,"accepted_min"))<0||uintcmp(number(sc,"target"),number(sc,"accepted_max"))>0)bad("schema_invalid")
    deps=array(id,"depends_on");unique_strings(deps,"roles_invalid",0)
    for(i=1;i<=count[deps];i++){k=val[member[deps,i]];if(k==role||k!~/^(app|service_host|server|worker|postgresql|extension)$/)bad("roles_invalid");edges[pi,role,k]=1}
    if(count[array(id,"required_extensions")]!=0)bad("extensions_unsupported")
    own=object(id,"ownership");exact(own,"package_owned_paths private_payload_paths");part=0
    for(j=1;j<=2;j++){arr=array(own,j==1?"package_owned_paths":"private_payload_paths");unique_strings(arr,"inventory_invalid",1);for(i=1;i<=count[arr];i++){p=val[member[arr,i]];if(!owned[id,p]||partition[id,p]++)bad("inventory_invalid");part++}}
    if(part!=count[inv])bad("inventory_invalid")
    if(role=="postgresql") {
      pg=object(id,"postgresql");exact(pg,"major version distribution build runtime_libraries data_layout_version")
      if(number(pg,"major")!=16||str(pg,"version")!=str(id,"version")||str(pg,"version")!~/^16\./||str(pg,"distribution")==""||str(pg,"build")==""||number(pg,"data_layout_version")==0)bad("postgresql_invalid")
      unique_strings(array(pg,"runtime_libraries"),"postgresql_invalid",1)
    } else if(member[id,"postgresql"])bad("postgresql_invalid")
    if(role=="app"&&selected==pi){put("asset",str(a,"name"));put("size",sz);put("sha256",str(a,"sha256"));put("format",str(a,"format"))}
  }
  function document(id,  fields,a,i,x,j,k,p,ps,os,arch,cs,c,r,srcobj,pr,rb,rs,t,rr,u,v,z) {
    fields="format_version critical kind issued_at expires_at root_sequence"
    if(wantkind=="delegation")fields=fields" delegation_sequence keys revoked_key_ids"
    else if(wantkind=="root_rotation")fields=fields" new_sequence new_root"
    else if(wantkind=="component_set")fields=fields" delegation_sequence release_sequence release_tag channel source provenance license sbom rollback platforms"
    else bad("wrong_kind")
    common(id,fields)
    if(wantkind=="delegation") {
      put("delegation_sequence",positive(id,"delegation_sequence"));a=array(id,"keys");if(!count[a])bad("invalid_document");put("keys_count",count[a])
      for(i=1;i<=count[a];i++){x=member[a,i];keyobj(x,"key"i);k=str(x,"key_id");if(seen[k]++)bad("invalid_key")}
      a=array(id,"revoked_key_ids");unique_strings(a,"invalid_key",0);r=""
      for(i=1;i<=count[a];i++){k=val[member[a,i]];if(!h64(k))bad("invalid_key");r=r k "\n"};put("revoked",r)
    } else if(wantkind=="root_rotation") { put("new_sequence",positive(id,"new_sequence"));keyobj(object(id,"new_root"),"new") }
    else {
      if((wantos=="")!=(wantarch==""))bad("wrong_platform")
      put("delegation_sequence",positive(id,"delegation_sequence"));rs=positive(id,"release_sequence");put("release_sequence",rs)
      t=str(id,"release_tag");if(t!~/^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/||(wanttag!=""&&t!=wanttag))bad("wrong_tag");put("tag",t)
      if(str(id,"channel")!="stable")bad("invalid_document")
      srcobj=object(id,"source");exact(srcobj,"repository commit");k=str(srcobj,"commit");if(str(srcobj,"repository")==""||length(k)!=40||k~/[^a-f0-9]/)bad("invalid_document")
      pr=object(id,"provenance");exact(pr,"checksums_sha256 sigstore_bundle");if(!h64(str(pr,"checksums_sha256")))bad("invalid_document");path(pr,"sigstore_bundle");path(id,"license");path(id,"sbom")
      rb=object(id,"rollback");exact(rb,"allowed_targets");a=array(rb,"allowed_targets");r=""
      for(i=1;i<=count[a];i++){x=member[a,i];if(type[x]!="n"||val[x]==0||uintcmp(val[x],rs)>=0||targets[val[x]]++)bad("sequence_invalid");r=r val[x] "\n"};put("rollback",r)
      ps=array(id,"platforms");if(count[ps]<1||count[ps]>6)bad("wrong_platform");selected=0
      for(i=1;i<=count[ps];i++) {
        p=member[ps,i];exact(p,"os arch min_os glibc_floor components");os=str(p,"os");arch=str(p,"arch")
        if((os!="linux"&&os!="darwin"&&os!="windows")||(arch!="amd64"&&arch!="arm64")||platform[os,arch]++||str(p,"min_os")=="")bad("wrong_platform")
        if(os=="linux"){if(str(p,"glibc_floor")!~/^2\.[0-9][0-9]$/)bad("wrong_platform")}else if(member[p,"glibc_floor"]&&str(p,"glibc_floor")!="")bad("wrong_platform")
        if((wantos==""||wantos==os)&&(wantarch==""||wantarch==arch))selected=i
        cs=array(p,"components");for(j=1;j<=count[cs];j++)component(member[cs,j],i,j)
        if(roles[i,"app"]!=1||roles[i,"service_host"]!=1||roles[i,"server"]!=1||roles[i,"postgresql"]!=1||roles[i,"worker"]<1)bad("roles_invalid")
        split("app service_host server worker postgresql extension",rr," ")
        for(u=1;u<=6;u++)for(v=1;v<=6;v++)if(edges[i,rr[u],rr[v]]&&(!roles[i,rr[u]]||!roles[i,rr[v]]))bad("roles_invalid")
        for(z=1;z<=6;z++)for(u=1;u<=6;u++)for(v=1;v<=6;v++)if(edges[i,rr[u],rr[z]]&&edges[i,rr[z],rr[v]])edges[i,rr[u],rr[v]]=1
        for(u=1;u<=6;u++)if(edges[i,rr[u],rr[u]])bad("roles_invalid")
        for(u=1;u<=pathcount[i];u++)for(v=1;v<=pathcount[i];v++)if(u!=v&&index(paths[i,v],paths[i,u]"/")==1)bad("inventory_invalid")
      }
      if(!selected)bad("wrong_platform")
    }
  }
  function optional(id,k){return val[member[id,k]]}
  function indexcases(id,  a,i,c,j,q,r,st,x,sep) {
    a=array(id,"cases")
    for(i=1;i<=count[a];i++){
      c=member[a,i];r="";q=array(c,"roots");for(j=1;j<=count[q];j++)r=r (j>1?",":"") val[member[q,j]]
      st="";q=array(c,"setup");for(j=1;j<=count[q];j++){x=member[q,j];st=st (j>1?";":"") str(x,"kind") "," str(x,"document") "," str(x,"envelope")}
      print str(c,"name") "|" str(c,"kind") "|" str(c,"document") "|" str(c,"envelope") "|" str(c,"expected_reason") "|" optional(c,"positive") "|" optional(c,"untrusted_clock") "|" optional(c,"allow_rollback") "|" optional(c,"expected_tag") "|" optional(c,"os") "|" optional(c,"arch") "|" optional(c,"now") "|" r "|" st "|" optional(c,"negative_signature")
    }
  }
  BEGIN { for(i=0;i<256;i++)byte[sprintf("%c",i)]=i;pos=1 }
  { src=src $0 "\n" }
  END { if(failed)exit 1;root=parse(1);white();if(pos<=length(src)||type[root]!="{")bad("invalid_json");if(mode=="envelope")envelope(root);else if(mode=="document")document(root);else if(mode=="index")indexcases(root);else bad("invalid_document") }
  ' "$1"
}

ws_cs_key() { # SPKI base64 file, output PEM, output id
  ws_cs_b64=$(cat "$1")
  case "$ws_cs_b64" in ''|*[!A-Za-z0-9+/=]*) ws_cs_reason invalid_encoding; return 1;; esac
  printf '%s' "$ws_cs_b64" | openssl base64 -d -A >"$2.der" 2>/dev/null || { ws_cs_reason invalid_encoding; return 1; }
  [ "$(openssl base64 -A <"$2.der" 2>/dev/null)" = "$ws_cs_b64" ] || { ws_cs_reason invalid_encoding; return 1; }
  openssl pkey -pubin -inform DER -in "$2.der" -out "$2" 2>/dev/null || { ws_cs_reason invalid_key; return 1; }
  openssl pkey -pubin -in "$2" -outform DER -out "$2.roundtrip" 2>/dev/null || { ws_cs_reason invalid_key; return 1; }
  cmp -s "$2.der" "$2.roundtrip" || { ws_cs_reason invalid_key; return 1; }
  openssl rsa -pubin -in "$2" -text -noout >"$2.fields" 2>/dev/null || { ws_cs_reason invalid_key; return 1; }
  if ! grep -Eq 'Public-Key: \(3072 bit\)' "$2.fields" || ! grep -Eq 'Exponent: 65537 \(0x10001\)' "$2.fields"; then
    ws_cs_reason invalid_key; return 1
  fi
  openssl dgst -sha256 "$2.der" 2>/dev/null | sed 's/^.*= //' >"$3" || { ws_cs_reason filesystem_error; return 1; }
  grep -Eq '^[0-9a-f]{64}$' "$3" || { ws_cs_reason invalid_key; return 1; }
}

ws_cs_init_impl() { # private state directory, explicit test-only root PEM
  command -v openssl >/dev/null 2>&1 || { printf '%s\n' 'bootstrap verification requires openssl' >&2; ws_cs_reason invalid_key; return 1; }
  [ -n "$2" ] || { ws_cs_reason no_trust_root; return 1; }
  ws_cs_size "$2" 8192 || { ws_cs_reason invalid_key; return 1; }
  mkdir -p "$1/roots" "$1/keys" "$1/root-history" || { ws_cs_reason filesystem_error; return 1; }
  # Public fixture wrappers are intentionally TEST-ONLY; no private material is read.
  sed '/^-----BEGIN \(TEST-ONLY \)\{0,1\}PUBLIC KEY-----$/d; /^-----END \(TEST-ONLY \)\{0,1\}PUBLIC KEY-----$/d' "$2" | tr -d '\r\n' >"$1/root.b64" || { ws_cs_reason filesystem_error; return 1; }
  ws_cs_key "$1/root.b64" "$1/root.pem" "$1/root.id" || return 1
  ws_cs_root_id=$(cat "$1/root.id")
  cp "$1/root.pem" "$1/roots/$ws_cs_root_id.pem" || { ws_cs_reason filesystem_error; return 1; }
  cp "$1/root.pem" "$1/root-history/$ws_cs_root_id.pem" || { ws_cs_reason filesystem_error; return 1; }
  printf '1\n' >"$1/root_sequence" || { ws_cs_reason filesystem_error; return 1; }
  printf '0\n' >"$1/delegation_sequence" || { ws_cs_reason filesystem_error; return 1; }
  printf '0\n' >"$1/high_release" || { ws_cs_reason filesystem_error; return 1; }
  : >"$1/rollback" || { ws_cs_reason filesystem_error; return 1; }
  : >"$1/revoked" || { ws_cs_reason filesystem_error; return 1; }
}

ws_cs_signature() { # signature b64 file, trusted PEM, exact document, scratch raw
  ws_cs_b64=$(cat "$1")
  case "$ws_cs_b64" in ''|*[!A-Za-z0-9+/=]*) ws_cs_reason invalid_encoding; return 1;; esac
  printf '%s' "$ws_cs_b64" | openssl base64 -d -A >"$4" 2>/dev/null || { ws_cs_reason invalid_encoding; return 1; }
  [ "$(openssl base64 -A <"$4" 2>/dev/null)" = "$ws_cs_b64" ] || { ws_cs_reason invalid_encoding; return 1; }
  [ "$(wc -c <"$4" | tr -d '[:space:]')" = 384 ] || { ws_cs_reason signature_length; return 1; }
  openssl dgst -sha256 -verify "$2" -signature "$4" "$3" >/dev/null 2>&1 || { ws_cs_reason signature_invalid; return 1; }
}

# The strict parser constrains timestamps to YYYY-MM-DDTHH:MM:SSZ before use.
ws_cs_str_lt() {
  [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | LC_ALL=C sort | sed -n 1p)" = "$1" ]
}

# Compare decimal uint64 values without shell signed arithmetic or awk doubles.
ws_cs_uint() { # left relation right
  awk -v a="$1" -v op="$2" -v b="$3" 'BEGIN {
    c=length(a)!=length(b)?(length(a)<length(b)?-1:1):("x"a)==("x"b)?0:("x"a)<("x"b)?-1:1
    exit !(op=="lt"?c<0:op=="ge"?c>=0:c==0)
  }'
}
ws_cs_increment() {
  [ "$1" != 18446744073709551615 ] || return 1
  awk -v n="$1" 'BEGIN { s="";carry=1;for(i=length(n);i>0;i--){d=substr(n,i,1)+carry;carry=d==10;s=(d%10)s}print (carry?"1":"")s }'
}

ws_cs_apply_impl() { # state, kind, exact document, envelope (fresh acquisition only)
  ws_cs_state=$1; ws_cs_kind=$2; ws_cs_doc=$3; ws_cs_env=$4
  ws_cs_limit=16384; [ "$ws_cs_kind" != component_set ] || ws_cs_limit=262144
  ws_cs_size "$ws_cs_doc" "$ws_cs_limit" || { ws_cs_reason document_too_large; return 1; }
  ws_cs_size "$ws_cs_env" 8192 || { ws_cs_reason envelope_too_large; return 1; }
  ws_cs_stage="$ws_cs_state/stage"
  rm -rf "$ws_cs_stage" || { ws_cs_reason filesystem_error; return 1; }
  mkdir "$ws_cs_stage" || { ws_cs_reason filesystem_error; return 1; }
  # Snapshot bounded exact bytes before authentication so path replacement cannot
  # turn the later parser into an unauthenticated input channel.
  dd if="$ws_cs_doc" of="$ws_cs_stage/document" bs=262145 count=1 2>/dev/null || { ws_cs_reason filesystem_error; return 1; }
  dd if="$ws_cs_env" of="$ws_cs_stage/envelope" bs=8193 count=1 2>/dev/null || { ws_cs_reason filesystem_error; return 1; }
  ws_cs_doc="$ws_cs_stage/document"; ws_cs_env="$ws_cs_stage/envelope"
  ws_cs_size "$ws_cs_doc" "$ws_cs_limit" || { ws_cs_reason document_too_large; return 1; }
  ws_cs_size "$ws_cs_env" 8192 || { ws_cs_reason envelope_too_large; return 1; }
  ws_cs_json "$ws_cs_env" envelope "$ws_cs_stage" "$ws_cs_kind" || return 1
  ws_cs_sig1=$(cat "$ws_cs_stage/sig1.id")
  ws_cs_signer=$ws_cs_sig1; ws_cs_sig_index=1
  if [ "$ws_cs_kind" = root_rotation ]; then
    ws_cs_sig2=$(cat "$ws_cs_stage/sig2.id")
    if [ ! -f "$ws_cs_state/roots/$ws_cs_signer.pem" ]; then ws_cs_signer=$ws_cs_sig2; ws_cs_sig_index=2; fi
  fi
  if [ "$ws_cs_kind" = component_set ]; then
    grep -Fxq "$ws_cs_signer" "$ws_cs_state/revoked" && { ws_cs_reason revoked_key; return 1; }
    [ ! -f "$ws_cs_state/root-history/$ws_cs_signer.pem" ] || { ws_cs_reason wrong_signer; return 1; }
    ws_cs_signer_file="$ws_cs_state/keys/$ws_cs_signer.pem"
  else
    [ ! -f "$ws_cs_state/keys/$ws_cs_signer.pem" ] || { ws_cs_reason wrong_signer; return 1; }
    ws_cs_signer_file="$ws_cs_state/roots/$ws_cs_signer.pem"
  fi
  [ -f "$ws_cs_signer_file" ] || { ws_cs_reason unknown_key; return 1; }
  ws_cs_signature "$ws_cs_stage/sig$ws_cs_sig_index.b64" "$ws_cs_signer_file" "$ws_cs_doc" "$ws_cs_stage/raw.sig" || return 1
  [ "${WS_UNTRUSTED_CLOCK:-0}" != 1 ] || { ws_cs_reason unknown_clock; return 1; }
  ws_cs_json "$ws_cs_doc" document "$ws_cs_stage" "$ws_cs_kind" || return 1
  [ "$(cat "$ws_cs_stage/root_sequence")" = "$(cat "$ws_cs_state/root_sequence")" ] || { ws_cs_reason sequence_invalid; return 1; }
  case "$ws_cs_kind" in
    root_rotation)
      ws_cs_new=$(cat "$ws_cs_stage/new.id")
      [ ! -f "$ws_cs_state/root-history/$ws_cs_new.pem" ] || { ws_cs_reason invalid_key; return 1; }
      ws_cs_next=$(ws_cs_increment "$(cat "$ws_cs_state/root_sequence")") && [ "$(cat "$ws_cs_stage/new_sequence")" = "$ws_cs_next" ] || { ws_cs_reason sequence_invalid; return 1; }
      ws_cs_key "$ws_cs_stage/new.spki" "$ws_cs_stage/new.pem" "$ws_cs_stage/new.actual" || return 1
      cmp -s "$ws_cs_stage/new.id" "$ws_cs_stage/new.actual" || { ws_cs_reason invalid_key; return 1; }
      if [ "$ws_cs_sig_index" = 1 ]; then ws_cs_other=$ws_cs_sig2; ws_cs_other_index=2; else ws_cs_other=$ws_cs_sig1; ws_cs_other_index=1; fi
      [ "$ws_cs_other" = "$ws_cs_new" ] || { ws_cs_reason wrong_signer; return 1; }
      ws_cs_signature "$ws_cs_stage/sig$ws_cs_other_index.b64" "$ws_cs_stage/new.pem" "$ws_cs_doc" "$ws_cs_stage/new.sig" || return 1
      rm -f "$ws_cs_state/roots/"*.pem "$ws_cs_state/keys/"*.pem || { ws_cs_reason filesystem_error; return 1; }
      cp "$ws_cs_stage/new.pem" "$ws_cs_state/roots/$ws_cs_new.pem" || { ws_cs_reason filesystem_error; return 1; }
      cp "$ws_cs_stage/new.pem" "$ws_cs_state/root-history/$ws_cs_new.pem" || { ws_cs_reason filesystem_error; return 1; }
      cp "$ws_cs_stage/new_sequence" "$ws_cs_state/root_sequence" || { ws_cs_reason filesystem_error; return 1; }
      printf '0\n' >"$ws_cs_state/delegation_sequence" || { ws_cs_reason filesystem_error; return 1; }
      ;;
    delegation)
      ws_cs_seq=$(cat "$ws_cs_stage/delegation_sequence"); ws_cs_old=$(cat "$ws_cs_state/delegation_sequence")
      ws_cs_uint "$ws_cs_seq" ge "$ws_cs_old" || { ws_cs_reason replay; return 1; }
      if [ "$ws_cs_seq" = "$ws_cs_old" ] && ! cmp -s "$ws_cs_doc" "$ws_cs_state/delegation.document"; then ws_cs_reason replay; return 1; fi
      ws_cs_i=1
      while [ "$ws_cs_i" -le "$(cat "$ws_cs_stage/keys_count")" ]; do
        ws_cs_id=$(cat "$ws_cs_stage/key$ws_cs_i.id")
        [ ! -f "$ws_cs_state/root-history/$ws_cs_id.pem" ] || { ws_cs_reason wrong_signer; return 1; }
        ws_cs_key "$ws_cs_stage/key$ws_cs_i.spki" "$ws_cs_stage/key$ws_cs_i.pem" "$ws_cs_stage/key$ws_cs_i.actual" || return 1
        cmp -s "$ws_cs_stage/key$ws_cs_i.id" "$ws_cs_stage/key$ws_cs_i.actual" || { ws_cs_reason invalid_key; return 1; }
        ws_cs_i=$((ws_cs_i + 1))
      done
      rm -f "$ws_cs_state/keys/"*.pem || { ws_cs_reason filesystem_error; return 1; }
      ws_cs_i=1
      while [ "$ws_cs_i" -le "$(cat "$ws_cs_stage/keys_count")" ]; do
        ws_cs_id=$(cat "$ws_cs_stage/key$ws_cs_i.id")
        cp "$ws_cs_stage/key$ws_cs_i.pem" "$ws_cs_state/keys/$ws_cs_id.pem" || { ws_cs_reason filesystem_error; return 1; }
        ws_cs_i=$((ws_cs_i + 1))
      done
      cp "$ws_cs_stage/delegation_sequence" "$ws_cs_state/delegation_sequence" || { ws_cs_reason filesystem_error; return 1; }
      cp "$ws_cs_stage/revoked" "$ws_cs_state/revoked" || { ws_cs_reason filesystem_error; return 1; }
      cp "$ws_cs_stage/issued" "$ws_cs_state/delegation.issued" || { ws_cs_reason filesystem_error; return 1; }
      cp "$ws_cs_stage/expires" "$ws_cs_state/delegation.expires" || { ws_cs_reason filesystem_error; return 1; }
      cp "$ws_cs_doc" "$ws_cs_state/delegation.document" || { ws_cs_reason filesystem_error; return 1; }
      ;;
    component_set)
      [ "$(cat "$ws_cs_stage/delegation_sequence")" = "$(cat "$ws_cs_state/delegation_sequence")" ] || { ws_cs_reason sequence_invalid; return 1; }
      if ws_cs_str_lt "$(cat "$ws_cs_stage/issued")" "$(cat "$ws_cs_state/delegation.issued")"; then ws_cs_reason invalid_document; return 1; fi
      if ws_cs_str_lt "$(cat "$ws_cs_state/delegation.expires")" "$(cat "$ws_cs_stage/expires")"; then ws_cs_reason invalid_document; return 1; fi
      ws_cs_str_lt "$WS_CS_NOW" "$(cat "$ws_cs_state/delegation.expires")" || { ws_cs_reason expired; return 1; }
      ws_cs_seq=$(cat "$ws_cs_stage/release_sequence"); ws_cs_high=$(cat "$ws_cs_state/high_release")
      if ws_cs_uint "$ws_cs_seq" lt "$ws_cs_high"; then
        [ "${WS_CS_ALLOW_ROLLBACK:-0}" = 1 ] || { ws_cs_reason replay; return 1; }
        grep -Fxq "$ws_cs_seq" "$ws_cs_state/rollback" || { ws_cs_reason rollback_denied; return 1; }
      elif [ "$ws_cs_seq" = "$ws_cs_high" ]; then
        cmp -s "$ws_cs_doc" "$ws_cs_state/component.document" || { ws_cs_reason replay; return 1; }
      else
        cp "$ws_cs_stage/release_sequence" "$ws_cs_state/high_release" || { ws_cs_reason filesystem_error; return 1; }
        cp "$ws_cs_stage/rollback" "$ws_cs_state/rollback" || { ws_cs_reason filesystem_error; return 1; }
        cp "$ws_cs_doc" "$ws_cs_state/component.document" || { ws_cs_reason filesystem_error; return 1; }
      fi
      ;;
  esac
  printf 'accepted\n'
}

ws_cs_bundle_impl() { # component-set dir, private scratch, explicit test root, tag, os, arch
  WS_CS_TAG=$4; WS_CS_OS=$5; WS_CS_ARCH=$6; WS_CS_ALLOW_ROLLBACK=0
  WS_CS_NOW=$(date -u '+%Y-%m-%dT%H:%M:%SZ') || { ws_cs_reason unknown_clock; return 1; }
  ws_cs_bundle_dir=$1; ws_cs_bundle_state=$2
  ws_cs_init "$2" "$3" || return 1
  # Validate the supplied finite rotation chain before accepting any authority.
  for ws_cs_rotation_file in "$ws_cs_bundle_dir"/root-rotation-*.json; do
    [ -e "$ws_cs_rotation_file" ] || continue
    case "$ws_cs_rotation_file" in *.sig.json) continue;; esac
    ws_cs_rotation_name=${ws_cs_rotation_file##*/}
    ws_cs_rotation_number=${ws_cs_rotation_name#root-rotation-}
    ws_cs_rotation_number=${ws_cs_rotation_number%.json}
    case "$ws_cs_rotation_number" in ""|0*|*[!0-9]*) ws_cs_reason sequence_invalid; return 1;; esac
    [ "${#ws_cs_rotation_number}" -le 2 ] && [ "$ws_cs_rotation_number" -le 32 ] || { ws_cs_reason sequence_invalid; return 1; }
    ws_cs_previous=1
    while [ "$ws_cs_previous" -lt "$ws_cs_rotation_number" ]; do
      [ -f "$ws_cs_bundle_dir/root-rotation-$ws_cs_previous.json" ] || { ws_cs_reason sequence_invalid; return 1; }
      ws_cs_previous=$((ws_cs_previous+1))
    done
  done
  ws_cs_rotation=1
  while [ -f "$ws_cs_bundle_dir/root-rotation-$ws_cs_rotation.json" ]; do
    ws_cs_apply "$ws_cs_bundle_state" root_rotation "$ws_cs_bundle_dir/root-rotation-$ws_cs_rotation.json" "$ws_cs_bundle_dir/root-rotation-$ws_cs_rotation.json.sig.json" >"$ws_cs_bundle_state/result" || { cat "$ws_cs_bundle_state/result"; return 1; }
    ws_cs_rotation=$((ws_cs_rotation+1))
  done
  ws_cs_apply "$ws_cs_bundle_state" delegation "$ws_cs_bundle_dir/delegation.json" "$ws_cs_bundle_dir/delegation.json.sig.json" >"$ws_cs_bundle_state/result" || { cat "$ws_cs_bundle_state/result"; return 1; }
  ws_cs_apply "$ws_cs_bundle_state" component_set "$ws_cs_bundle_dir/component-set.json" "$ws_cs_bundle_dir/component-set.json.sig.json" >"$ws_cs_bundle_state/result" || { cat "$ws_cs_bundle_state/result"; return 1; }
}

ws_cs_archive_impl() { # state from the single accepted manifest, archive
  [ -f "$2" ] && [ ! -L "$2" ] || { ws_cs_reason filesystem_error; return 1; }
  [ -s "$1/stage/size" ] && [ -s "$1/stage/sha256" ] || { ws_cs_reason mixed_set; return 1; }
  ws_cs_expected_size=$(cat "$1/stage/size") || { ws_cs_reason filesystem_error; return 1; }
  ws_cs_expected_digest=$(cat "$1/stage/sha256") || { ws_cs_reason filesystem_error; return 1; }
  case "$ws_cs_expected_size" in ''|0*|*[!0-9]*) ws_cs_reason mixed_set; return 1;; esac
  [ "${#ws_cs_expected_digest}" = 64 ] || { ws_cs_reason mixed_set; return 1; }
  case "$ws_cs_expected_digest" in *[!0-9a-f]*) ws_cs_reason mixed_set; return 1;; esac
  [ "$(wc -c <"$2" | tr -d '[:space:]')" = "$ws_cs_expected_size" ] || { ws_cs_reason archive_size_mismatch; return 1; }
  ws_cs_digest=$(openssl dgst -sha256 "$2" 2>/dev/null | sed 's/^.*= //')
  [ "$ws_cs_digest" = "$ws_cs_expected_digest" ] || { ws_cs_reason archive_digest_mismatch; return 1; }
}

ws_cs_guard() {
  if ws_cs_result=$("$@" 2>/dev/null); then
    case "$ws_cs_result" in ""|accepted) [ -z "$ws_cs_result" ] || printf 'accepted\n'; return 0;; esac
  else
    case "$ws_cs_result" in
      no_trust_root|document_too_large|envelope_too_large|invalid_json|duplicate_key|profile_violation|unknown_critical|unknown_field|invalid_document|wrong_kind|wrong_algorithm|invalid_key|invalid_encoding|signature_length|signature_count|signature_invalid|unknown_key|revoked_key|wrong_signer|state_invalid|state_io|sequence_invalid|replay|rollback_denied|wrong_tag|wrong_platform|schema_invalid|postgresql_invalid|extensions_unsupported|roles_invalid|path_invalid|inventory_invalid|mode_invalid|not_installed|mixed_set|expired|not_yet_valid|unknown_clock|invalid_inventory|unsafe_path|unsupported_archive|archive_size_mismatch|archive_digest_mismatch|archive_invalid|archive_entry_type|archive_duplicate_entry|archive_undeclared_entry|archive_missing_entry|entry_size_mismatch|entry_digest_mismatch|entry_mode_mismatch|inventory_limit|staging_not_fresh|filesystem_error|cancelled) ws_cs_reason "$ws_cs_result"; return 1;;
    esac
  fi
  ws_cs_reason filesystem_error
}
ws_cs_init() {
  if ! command -v openssl >/dev/null 2>&1; then
    if [ "${ws_cs_error_fd_ready:-0}" = 1 ]; then printf 'bootstrap verification requires openssl\n' >&3
    else printf 'bootstrap verification requires openssl\n' >&2; fi
    ws_cs_reason invalid_key; return 1
  fi
  ws_cs_guard ws_cs_init_impl "$@"
}
ws_cs_apply() { ws_cs_guard ws_cs_apply_impl "$@"; }
ws_cs_bundle() {
  if ! command -v openssl >/dev/null 2>&1; then
    if [ "${ws_cs_error_fd_ready:-0}" = 1 ]; then printf 'bootstrap verification requires openssl\n' >&3
    else printf 'bootstrap verification requires openssl\n' >&2; fi
    ws_cs_reason invalid_key; return 1
  fi
  ws_cs_guard ws_cs_bundle_impl "$@"
}
ws_cs_archive() { ws_cs_guard ws_cs_archive_impl "$@"; }

# Parity tests source precisely these functions without running an installation.
if [ "${WS_BOOTSTRAP_LIBRARY:-0}" = 1 ]; then return 0; fi

set -eu

REPO="witwave-ai/witself"
INSTALL_DIR="${WS_INSTALL_DIR:-/usr/local/bin}"
RELEASE_DIR="${WS_RELEASE_DIR:-}"

err() {
  if [ -n "${WS_COMPONENT_SET_DIR:-}" ]; then
    case "$1" in
      no_trust_root|document_too_large|envelope_too_large|invalid_json|duplicate_key|profile_violation|unknown_critical|unknown_field|invalid_document|wrong_kind|wrong_algorithm|invalid_key|invalid_encoding|signature_length|signature_count|signature_invalid|unknown_key|revoked_key|wrong_signer|state_invalid|state_io|sequence_invalid|replay|rollback_denied|wrong_tag|wrong_platform|schema_invalid|postgresql_invalid|extensions_unsupported|roles_invalid|path_invalid|inventory_invalid|mode_invalid|not_installed|mixed_set|expired|not_yet_valid|unknown_clock|invalid_inventory|unsafe_path|unsupported_archive|archive_size_mismatch|archive_digest_mismatch|archive_invalid|archive_entry_type|archive_duplicate_entry|archive_undeclared_entry|archive_missing_entry|entry_size_mismatch|entry_digest_mismatch|entry_mode_mismatch|inventory_limit|staging_not_fresh|filesystem_error|cancelled) printf 'install: %s\n' "$1" >&3;;
      *) printf 'install: filesystem_error\n' >&3;;
    esac
  else printf 'install: %s\n' "$1" >&2; fi
  exit 1
}
info() { if [ -z "${WS_COMPONENT_SET_DIR:-}" ]; then printf '%s\n' "$1" >&2; fi; }
have() { command -v "$1" >/dev/null 2>&1; }
if [ -n "${WS_COMPONENT_SET_DIR:-}" ]; then
  exec 3>&2 2>/dev/null
  ws_cs_error_fd_ready=1
fi

BINARY="${WITSELF_BINARY:-witself}"
version="${WS_VERSION:-}"

case "${1:-}" in
  "")
    [ "$#" -eq 0 ] || err "empty binary/version argument"
    ;;
  witself | ws | witself-infra | witself-server | witself-admin)
    BINARY="$1"
    version="${2:-${WS_VERSION:-}}"
    [ "$#" -le 2 ] || err "too many arguments (usage: sh -s [BINARY] [VERSION])"
    ;;
  *)
    version="$1"
    [ "$#" -le 1 ] || err "too many arguments (usage: sh -s [BINARY] [VERSION])"
    ;;
esac

# "ws" is the muscle-memory alias for the renamed tenant CLI.
[ "$BINARY" = "ws" ] && BINARY="witself"

case "$BINARY" in
  witself | witself-infra | witself-server | witself-admin) ;;
  *) err "unknown binary \"${BINARY}\" (want witself|witself-infra|witself-server|witself-admin)" ;;
esac

download() { # url dest
  if have curl; then curl -fsSL "$1" -o "$2"
  elif have wget; then wget -qO "$2" "$1"
  else err "need curl or wget"; fi
}
fetch() { # url -> stdout
  if have curl; then curl -fsSL "$1"
  elif have wget; then wget -qO- "$1"
  else err "need curl or wget"; fi
}

# Detect OS and architecture.
os=$(uname -s)
case "$os" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) err "unsupported OS: $os (linux and darwin only)" ;;
esac
arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) err "unsupported architecture: $arch (amd64 and arm64 only)" ;;
esac

# Explicit component sets require an explicit target and authenticate before any
# release lookup or payload download. Normal installs do not enter this branch.
component_scratch=""
if [ -n "${WS_COMPONENT_SET_DIR:-}" ]; then
  component_scratch=$(mktemp -d 2>/dev/null || mktemp -d -t ws-component)
  chmod 700 "$component_scratch"
  trap 'rm -rf "$component_scratch"' 0
  component_tag=$version
  case "$component_tag" in ""|v*) ;; *) component_tag="v$component_tag";; esac
  if [ -n "$component_tag" ]; then
    printf '%s\n' "$component_tag" | LC_ALL=C grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || err wrong_tag
  fi
  component_reason=$(ws_cs_bundle "$WS_COMPONENT_SET_DIR" "$component_scratch" "${WS_TRUST_ROOT_PEM_FILE:-}" "$component_tag" "$os" "$arch") \
    || err "${component_reason:-invalid_document}"
  version=$(cat "$component_scratch/stage/tag")
fi

# Resolve the version: positional arg, then WS_VERSION, then the latest release.
if [ -z "$version" ]; then
  [ -z "$RELEASE_DIR" ] || err "WS_RELEASE_DIR requires an explicit version"
  info "Resolving latest ${BINARY} release..."
  version=$(fetch "https://api.github.com/repos/${REPO}/releases/latest" |
    grep '"tag_name"' | head -1 | sed -e 's/.*"tag_name":[[:space:]]*"//' -e 's/".*//')
  [ -n "$version" ] || err "could not resolve the latest version"
fi
# The tag carries a leading v; the asset name uses the version without it.
case "$version" in
  v*) tag="$version"; ver="${version#v}" ;;
  *) tag="v$version"; ver="$version" ;;
esac
case "$ver" in
  "" | *[!A-Za-z0-9._+-]*) err "invalid version \"${version}\"" ;;
esac

asset="${BINARY}_${ver}_${os}_${arch}.tar.gz"
if [ -n "$component_scratch" ]; then
  [ "$BINARY" = witself ] || err wrong_platform
  asset=$(cat "$component_scratch/stage/asset")
  [ "$(cat "$component_scratch/stage/format")" = tar.gz ] || err unsupported_archive
fi
base="https://github.com/${REPO}/releases/download/${tag}"

if [ -n "$RELEASE_DIR" ]; then
  case "$RELEASE_DIR" in
    /*) ;;
    *) err "WS_RELEASE_DIR must be an absolute path" ;;
  esac
  [ -d "$RELEASE_DIR" ] || err "WS_RELEASE_DIR is not a directory: ${RELEASE_DIR}"
  RELEASE_DIR=$(CDPATH='' cd -P "$RELEASE_DIR" 2>/dev/null && pwd) \
    || err "could not resolve WS_RELEASE_DIR: ${RELEASE_DIR}"
fi

info "Installing ${BINARY} ${tag} (${os}/${arch})..."

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t ws-install)

dest=""
dest_use_sudo=0
lock_dir=""
lock_owner_file=""
lock_held=0
txn_dir=""
staged_binary=""
staged_alias=""
primary_path=""
alias_path=""
primary_backup=""
primary_restore=""
alias_backup=""
alias_restore=""
primary_quarantine=""
alias_quarantine=""
prior_primary_kind="none"
prior_primary_identity=""
prior_primary_hash=""
prior_primary_link=""
prior_primary_runnable=0
prior_alias_kind="none"
prior_alias_identity=""
prior_alias_link=""
staged_binary_identity=""
staged_binary_hash=""
staged_alias_identity=""
commit_started=0
primary_replaced=0
alias_replaced=0
transaction_committed=0
preserve_transaction=0

run_dest() {
  if [ "$dest_use_sudo" -eq 1 ]; then
    sudo "$@"
  else
    "$@"
  fi
}

self_test_binary() { # path
  "$1" version >/dev/null 2>&1
}

version_output_matches_request() { # output
  case "$1" in
    "${BINARY} ${ver}" | "${BINARY} ${ver} ("*")") return 0 ;;
    *) return 1 ;;
  esac
}

destination_file_hash() { # path
  if have sha256sum; then
    run_dest sha256sum "$1" | awk '{print $1}'
  elif have shasum; then
    run_dest shasum -a 256 "$1" | awk '{print $1}'
  else
    return 1
  fi
}

path_identity() { # path
  run_dest ls -di "$1" 2>/dev/null | awk 'NR == 1 {print $1}'
}

target_matches_snapshot() { # path kind identity hash link
  snapshot_path=$1
  snapshot_kind=$2
  snapshot_identity=$3
  snapshot_hash=$4
  snapshot_link=$5
  case "$snapshot_kind" in
    none)
      [ ! -e "$snapshot_path" ] && [ ! -L "$snapshot_path" ]
      ;;
    file)
      [ -f "$snapshot_path" ] && [ ! -L "$snapshot_path" ] \
        && [ "$(path_identity "$snapshot_path")" = "$snapshot_identity" ] \
        && [ "$(destination_file_hash "$snapshot_path")" = "$snapshot_hash" ]
      ;;
    symlink)
      [ -L "$snapshot_path" ] \
        && [ "$(path_identity "$snapshot_path")" = "$snapshot_identity" ] \
        && [ "$(readlink "$snapshot_path")" = "$snapshot_link" ]
      ;;
    *)
      return 1
      ;;
  esac
}

restore_quarantined_path() { # canonical quarantine
  restore_canonical=$1
  restore_quarantine=$2
  if [ -L "$restore_quarantine" ]; then
    restore_link=$(readlink "$restore_quarantine") || return 1
    run_dest ln -s -- "$restore_link" "$restore_canonical" \
      && run_dest rm -f "$restore_quarantine"
    return
  fi
  if [ -f "$restore_quarantine" ]; then
    run_dest ln "$restore_quarantine" "$restore_canonical" \
      && run_dest rm -f "$restore_quarantine"
    return
  fi
  return 1
}

quarantine_installed_target() { # canonical quarantine expected-kind identity hash link
  quarantine_canonical=$1
  quarantine_path=$2
  quarantine_kind=$3
  quarantine_identity=$4
  quarantine_hash=$5
  quarantine_link=$6
  [ ! -e "$quarantine_path" ] && [ ! -L "$quarantine_path" ] || return 1
  [ -e "$quarantine_canonical" ] || [ -L "$quarantine_canonical" ] || return 1
  run_dest mv "$quarantine_canonical" "$quarantine_path" || return 1
  if target_matches_snapshot \
    "$quarantine_path" "$quarantine_kind" "$quarantine_identity" "$quarantine_hash" "$quarantine_link"; then
    return 0
  fi

  # The live path changed after commit. Put those later bytes back only when
  # their canonical name is still vacant; never overwrite another concurrent
  # writer. Returning failure retains the verified previous backup.
  restore_quarantined_path "$quarantine_canonical" "$quarantine_path" || :
  return 1
}

restore_prior_target() { # canonical prior-kind backup restore link
  restore_canonical=$1
  restore_kind=$2
  restore_backup=$3
  restore_stage=$4
  restore_link=$5
  [ ! -e "$restore_canonical" ] && [ ! -L "$restore_canonical" ] || return 1
  case "$restore_kind" in
    file)
      run_dest cp -p "$restore_backup" "$restore_stage" \
        && run_dest cmp -s "$restore_backup" "$restore_stage" \
        && run_dest ln "$restore_stage" "$restore_canonical" \
        && run_dest cmp -s "$restore_backup" "$restore_canonical"
      ;;
    symlink)
      run_dest ln -s -- "$restore_link" "$restore_canonical" \
        && [ -L "$restore_canonical" ] \
        && [ "$(readlink "$restore_canonical")" = "$restore_link" ]
      ;;
    none)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

rollback_target() { # canonical quarantine installed-kind identity hash link prior-kind backup restore prior-link
  rollback_canonical=$1
  rollback_quarantine=$2
  rollback_installed_kind=$3
  rollback_installed_identity=$4
  rollback_installed_hash=$5
  rollback_installed_link=$6
  rollback_prior_kind=$7
  rollback_backup=$8
  rollback_restore=$9
  shift 9
  rollback_prior_link=$1

  quarantine_installed_target \
    "$rollback_canonical" "$rollback_quarantine" \
    "$rollback_installed_kind" "$rollback_installed_identity" \
    "$rollback_installed_hash" "$rollback_installed_link" \
    || return 1
  if ! restore_prior_target \
    "$rollback_canonical" "$rollback_prior_kind" \
    "$rollback_backup" "$rollback_restore" "$rollback_prior_link"; then
    return 1
  fi
  run_dest rm -f "$rollback_quarantine"
}

rollback_install() {
  rollback_ok=1

  if [ "$primary_replaced" -eq 1 ]; then
    rollback_target \
      "$primary_path" "$primary_quarantine" file \
      "$staged_binary_identity" "$staged_binary_hash" "" \
      "$prior_primary_kind" "$primary_backup" "$primary_restore" "$prior_primary_link" \
      || rollback_ok=0
  fi

  if [ "$alias_replaced" -eq 1 ]; then
    rollback_target \
      "$alias_path" "$alias_quarantine" symlink \
      "$staged_alias_identity" "" "witself" \
      "$prior_alias_kind" "$alias_backup" "$alias_restore" "$prior_alias_link" \
      || rollback_ok=0
  fi

  if [ "$rollback_ok" -eq 1 ] && [ "$prior_primary_runnable" -eq 1 ]; then
    self_test_binary "$primary_path" || rollback_ok=0
  fi

  if [ "$rollback_ok" -eq 1 ]; then
    if [ "$prior_primary_kind" = "none" ]; then
      info "Removed failed ${BINARY} installation."
    else
      info "Restored the previous ${BINARY} installation."
    fi
    return 0
  fi

  info "WARNING: automatic rollback of ${BINARY} did not complete."
  info "Transaction recovery artifacts remain in ${txn_dir}; inspect them before retrying."
  return 1
}

cleanup() {
  cleanup_status=$1
  trap - 0 INT TERM HUP
  set +e

  if [ "$commit_started" -eq 1 ] && [ "$transaction_committed" -eq 0 ]; then
    if ! rollback_install; then
      cleanup_status=1
      preserve_transaction=1
    fi
  fi

  if [ "$preserve_transaction" -eq 0 ]; then
    for cleanup_path in \
      "$staged_binary" "$staged_alias" \
      "$primary_backup" "$primary_restore" \
      "$alias_backup" "$alias_restore" \
      "$primary_quarantine" "$alias_quarantine"
    do
      [ -n "$cleanup_path" ] && run_dest rm -f "$cleanup_path" >/dev/null 2>&1
    done
    if [ -n "$txn_dir" ]; then
      run_dest rmdir "$txn_dir" >/dev/null 2>&1
    fi
  fi

  if [ "$lock_held" -eq 1 ]; then
    run_dest rm -f "$lock_owner_file" >/dev/null 2>&1
    run_dest rmdir "$lock_dir" >/dev/null 2>&1
  fi

  rm -rf "$tmp"
  [ -z "$component_scratch" ] || rm -rf "$component_scratch"
  exit "$cleanup_status"
}

trap 'cleanup $?' 0
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

if [ -n "$RELEASE_DIR" ]; then
  for name in "$asset" checksums.txt; do
    source_path="${RELEASE_DIR}/${name}"
    [ -f "$source_path" ] && [ ! -L "$source_path" ] \
      || err "local release asset is missing or not a regular file: ${source_path}"
    cp "$source_path" "${tmp}/${name}" \
      || err "could not copy local release asset: ${source_path}"
  done
else
  download "${base}/${asset}" "${tmp}/${asset}"
  download "${base}/checksums.txt" "${tmp}/checksums.txt"
fi

# Verify the signed size and digest before any extraction or candidate execution.
if [ -n "$component_scratch" ]; then
  component_reason=$(ws_cs_archive "$component_scratch" "${tmp}/${asset}") || err "$component_reason"
fi

# Verify the SHA-256 checksum before trusting the binary.
expected=$(awk -v f="$asset" '$2 == f {print $1}' "${tmp}/checksums.txt")
[ -n "$expected" ] || err "no checksum found for ${asset}"
if have sha256sum; then actual=$(sha256sum "${tmp}/${asset}" | awk '{print $1}')
elif have shasum; then actual=$(shasum -a 256 "${tmp}/${asset}" | awk '{print $1}')
else err "need sha256sum or shasum to verify the download"; fi
[ "$expected" = "$actual" ] || err "checksum mismatch for ${asset} (expected ${expected}, got ${actual})"
info "Checksum verified."

# Extract.
archive_entries="${tmp}/archive-entries.txt"
archive_types="${tmp}/archive-types.txt"
LC_ALL=C tar -tzf "${tmp}/${asset}" >"${archive_entries}" \
  || err "could not inspect archive ${asset}"
LC_ALL=C tar -tvzf "${tmp}/${asset}" | cut -c1 >"${archive_types}" \
  || err "could not inspect archive entry types for ${asset}"
entry_count=$(wc -l <"${archive_entries}" | tr -d '[:space:]')
type_count=$(wc -l <"${archive_types}" | tr -d '[:space:]')
[ "$entry_count" = 1 ] && [ "$type_count" = 1 ] \
  && [ "$(sed -n '1p' "${archive_entries}")" = "$BINARY" ] \
  && [ "$(sed -n '1p' "${archive_types}")" = "-" ] \
  || err "archive ${asset} must contain exactly one regular root entry named ${BINARY}"
tar -xzf "${tmp}/${asset}" -C "${tmp}"
[ -f "${tmp}/${BINARY}" ] && [ ! -L "${tmp}/${BINARY}" ] \
  || err "binary ${BINARY} not found as a regular file in the archive"
chmod +x "${tmp}/${BINARY}"

# Select a destination before acquiring its installer-wide lock. The lock covers
# every Witself binary so concurrently installing different release assets cannot
# leave a partially updated command set or alias.
select_destination() {
  if mkdir -p "$INSTALL_DIR" 2>/dev/null && [ -d "$INSTALL_DIR" ] && [ -w "$INSTALL_DIR" ]; then
    dest="$INSTALL_DIR"
    dest_use_sudo=0
    return 0
  fi

  if have sudo && sudo mkdir -p "$INSTALL_DIR"; then
    info "Elevating with sudo to write $INSTALL_DIR..."
    dest="$INSTALL_DIR"
    dest_use_sudo=1
    return 0
  fi

  [ -n "${HOME:-}" ] || return 1
  fallback_dir="${HOME}/.local/bin"
  if mkdir -p "$fallback_dir" 2>/dev/null && [ -d "$fallback_dir" ] && [ -w "$fallback_dir" ]; then
    dest="$fallback_dir"
    dest_use_sudo=0
    info "Installing to ~/.local/bin — ensure it is on your PATH."
    return 0
  fi

  if have sudo && sudo mkdir -p "$fallback_dir"; then
    info "Elevating with sudo to write $fallback_dir..."
    dest="$fallback_dir"
    dest_use_sudo=1
    info "Installing to ~/.local/bin — ensure it is on your PATH."
    return 0
  fi

  return 1
}

acquire_install_lock() {
  lock_timeout="${WS_INSTALL_LOCK_TIMEOUT:-30}"
  case "$lock_timeout" in
    "" | *[!0-9]*) err "WS_INSTALL_LOCK_TIMEOUT must be a non-negative integer" ;;
  esac

  lock_dir="${dest}/.witself-install.lock"
  lock_owner_file="${lock_dir}/owner"
  lock_waited=0
  lock_announced=0
  current_uid=$(id -u 2>/dev/null || printf 'unknown')

  while :; do
    if run_dest mkdir "$lock_dir" 2>/dev/null; then
      lock_held=1
      printf '%s %s\n' "$$" "$current_uid" > "${tmp}/lock-owner"
      run_dest cp "${tmp}/lock-owner" "$lock_owner_file" \
        || err "could not record installer lock owner in ${lock_dir}"
      return 0
    fi

    # Reclaim only a same-user lock whose recorded process is definitely gone.
    # A missing or malformed owner file may be an installer between mkdir and cp,
    # so it is never treated as stale.
    lock_owner=$(run_dest cat "$lock_owner_file" 2>/dev/null || :)
    lock_pid=$(printf '%s\n' "$lock_owner" | awk 'NR == 1 {print $1}')
    lock_uid=$(printf '%s\n' "$lock_owner" | awk 'NR == 1 {print $2}')
    case "$lock_pid" in
      "" | *[!0-9]*) lock_pid="" ;;
    esac
    case "$lock_uid" in
      "" | *[!0-9]*) lock_uid="" ;;
    esac
    if [ -n "$lock_pid" ] && [ "$lock_uid" = "$current_uid" ] \
      && ! kill -0 "$lock_pid" 2>/dev/null; then
      info "Removing stale installer lock in ${dest}..."
      run_dest rm -f "$lock_owner_file" >/dev/null 2>&1 || :
      run_dest rmdir "$lock_dir" >/dev/null 2>&1 || :
      continue
    fi

    if [ "$lock_waited" -ge "$lock_timeout" ]; then
      err "another Witself installation is in progress for ${dest} (lock: ${lock_dir})"
    fi
    if [ "$lock_announced" -eq 0 ]; then
      info "Waiting for another Witself installation in ${dest}..."
      lock_announced=1
    fi
    sleep 1
    lock_waited=$((lock_waited + 1))
  done
}

select_destination || err "could not install to ${INSTALL_DIR} or ~/.local/bin"
dest=$(CDPATH='' cd -P "$dest" 2>/dev/null && pwd) \
  || err "could not resolve install directory: ${dest}"
acquire_install_lock

# Stage the executable in the destination filesystem and run it before changing
# any live path. The final mv operations are same-filesystem atomic renames.
txn_dir=$(run_dest mktemp -d "${dest}/.witself-install.XXXXXX") \
  || err "could not create an install transaction in ${dest}"
run_dest chmod 0755 "$txn_dir" \
  || err "could not prepare install transaction in ${dest}"
staged_binary="${txn_dir}/${BINARY}.candidate"
run_dest cp "${tmp}/${BINARY}" "$staged_binary" \
  || err "could not stage ${BINARY} in ${dest}"
run_dest chmod 0755 "$staged_binary" \
  || err "could not make staged ${BINARY} executable"
staged_version=$("$staged_binary" version 2>&1) \
  || err "staged ${BINARY} failed to run; the existing installation was not changed"
version_output_matches_request "$staged_version" \
  || err "staged ${BINARY} reported a version other than requested ${tag}; the existing installation was not changed"
staged_binary_identity=$(path_identity "$staged_binary") \
  || err "could not record staged ${BINARY} file identity"
staged_binary_hash=$(destination_file_hash "$staged_binary") \
  || err "could not hash staged ${BINARY}"
info "Staged ${BINARY} passed its runtime and version checks."

primary_path="${dest}/${BINARY}"
primary_backup="${txn_dir}/${BINARY}.previous"
primary_restore="${txn_dir}/${BINARY}.restore"
primary_quarantine="${txn_dir}/${BINARY}.rollback-target"
if [ -L "$primary_path" ]; then
  prior_primary_kind="symlink"
  prior_primary_identity=$(path_identity "$primary_path") \
    || err "could not record existing ${primary_path} identity"
  prior_primary_link=$(readlink "$primary_path") \
    || err "could not inspect existing ${primary_path}"
  run_dest ln -s -- "$prior_primary_link" "$primary_backup" \
    || err "could not preserve existing symlink ${primary_path}"
  target_matches_snapshot \
    "$primary_path" "$prior_primary_kind" "$prior_primary_identity" "" "$prior_primary_link" \
    || err "existing ${primary_path} changed while it was backed up"
  self_test_binary "$primary_path" && prior_primary_runnable=1 || :
elif [ -e "$primary_path" ]; then
  [ -f "$primary_path" ] \
    || err "existing install target is not a regular file: ${primary_path}"
  prior_primary_kind="file"
  prior_primary_identity=$(path_identity "$primary_path") \
    || err "could not record existing ${primary_path} identity"
  prior_primary_hash=$(destination_file_hash "$primary_path") \
    || err "could not hash existing ${primary_path}"
  if ! run_dest cp -p "$primary_path" "$primary_backup" \
    || ! run_dest cmp -s "$primary_path" "$primary_backup"; then
    err "could not make a verified backup of ${primary_path}"
  fi
  target_matches_snapshot \
    "$primary_path" "$prior_primary_kind" "$prior_primary_identity" "$prior_primary_hash" "" \
    || err "existing ${primary_path} changed while it was backed up"
  self_test_binary "$primary_path" && prior_primary_runnable=1 || :
fi

# Prepare and preserve the tenant alias before the first live rename. Only the
# exact ws -> witself symlink created by this installer is considered owned;
# every other pre-existing path is left untouched and requires user resolution.
if [ "$BINARY" = "witself" ]; then
  alias_path="${dest}/ws"
  alias_backup="${txn_dir}/ws.previous"
  alias_restore="${txn_dir}/ws.restore"
  alias_quarantine="${txn_dir}/ws.rollback-target"
  if [ -L "$alias_path" ]; then
    prior_alias_kind="symlink"
    prior_alias_identity=$(path_identity "$alias_path") \
      || err "could not record existing ${alias_path} identity"
    prior_alias_link=$(readlink "$alias_path") \
      || err "could not inspect existing ${alias_path}"
    [ "$prior_alias_link" = "witself" ] \
      || err "refusing to replace non-Witself alias at ${alias_path} (target: ${prior_alias_link})"
    run_dest ln -s -- "$prior_alias_link" "$alias_backup" \
      || err "could not preserve existing symlink ${alias_path}"
    target_matches_snapshot \
      "$alias_path" "$prior_alias_kind" "$prior_alias_identity" "" "$prior_alias_link" \
      || err "existing ${alias_path} changed while it was backed up"
  elif [ -e "$alias_path" ]; then
    err "refusing to replace non-Witself path at ${alias_path}"
  fi

  staged_alias="${txn_dir}/ws.candidate"
  run_dest ln -s -- "witself" "$staged_alias" \
    || err "could not stage the ws alias"
  staged_alias_identity=$(path_identity "$staged_alias") \
    || err "could not record staged ws alias identity"
fi

# Commit the primary and alias, then execute the committed paths while the
# verified backups and lock are still held. Any failure exits through cleanup,
# which restores the exact prior files (or removes a failed first install).
target_matches_snapshot \
  "$primary_path" "$prior_primary_kind" "$prior_primary_identity" "$prior_primary_hash" "$prior_primary_link" \
  || err "existing ${primary_path} changed before commit; the installation was not changed"
if [ "$BINARY" = "witself" ]; then
  target_matches_snapshot \
    "$alias_path" "$prior_alias_kind" "$prior_alias_identity" "" "$prior_alias_link" \
    || err "existing ${alias_path} changed before commit; the installation was not changed"
fi
commit_started=1
run_dest mv -f "$staged_binary" "$primary_path" \
  || err "could not atomically install ${BINARY} to ${primary_path}"
primary_replaced=1
target_matches_snapshot \
  "$primary_path" file "$staged_binary_identity" "$staged_binary_hash" "" \
  || err "installed ${BINARY} changed during commit; refusing unsafe rollback"
if [ "$BINARY" = "witself" ]; then
  run_dest mv -f "$staged_alias" "$alias_path" \
    || err "could not atomically install the ws alias"
  alias_replaced=1
  target_matches_snapshot \
    "$alias_path" symlink "$staged_alias_identity" "" "witself" \
    || err "installed ws alias changed during commit; refusing unsafe rollback"
fi

installed_version=$("$primary_path" version 2>&1) \
  || err "installed ${BINARY} failed to run after commit; restoring the previous installation"
version_output_matches_request "$installed_version" \
  || err "installed ${BINARY} reported a version other than requested ${tag}; restoring the previous installation"

if [ "$BINARY" = "witself" ]; then
  [ -L "$alias_path" ] && [ "$(readlink "$alias_path")" = "witself" ] \
    || err "installed ws alias is inconsistent; restoring the previous installation"
  alias_version=$("$alias_path" version 2>&1) \
    || err "installed ws alias failed to run; restoring the previous installation"
  version_output_matches_request "$alias_version" \
    || err "installed ws alias reported a version other than requested ${tag}; restoring the previous installation"
  [ "$alias_version" = "$installed_version" ] \
    || err "installed ws alias reported a different version from ${BINARY}; restoring the previous installation"
fi

transaction_committed=1
info "Installed ${BINARY} to ${primary_path}"
if [ "$BINARY" = "witself" ]; then
  info "Aliased ${alias_path} -> witself"
fi
if [ -n "$component_scratch" ]; then printf 'accepted\n'; else printf '%s\n' "$installed_version"; fi

if [ "$BINARY" = "witself-infra" ] && ! have pulumi; then
  info ""
  info "Note: witself-infra drives the 'pulumi' engine at runtime, which this"
  info "installer does not fetch. Install it with:  brew install pulumi"
  info "  (or: curl -fsSL https://get.pulumi.com | sh)"
fi
