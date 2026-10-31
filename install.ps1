#requires -Version 5.1
<#
.SYNOPSIS
Installs the native Windows x64 Witself CLI from a checksummed release archive.

.EXAMPLE
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12; irm https://raw.githubusercontent.com/witwave-ai/witself/main/install.ps1 | iex

.EXAMPLE
.\install.ps1 -Version v0.0.201 -NoPathUpdate

.NOTES
WS_INSTALL_LOCK_TIMEOUT_MS may bound how long concurrent installers wait for
the per-install-directory transaction lock (default: 30000).
#>
[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [string]$Version = $env:WS_VERSION,

    [string]$InstallDir = $env:WS_INSTALL_DIR,

    [switch]$NoPathUpdate,

    # An HTTPS release/download root or a local fixture root. Local roots use
    # <root>\<tag>\{asset,checksums.txt} and may provide <root>\latest.json.
    [string]$ReleaseRoot = $env:WS_RELEASE_ROOT,

    # Source only the dark verifier; used by the offline parity/unit suites.
    [switch]$VerifierOnly
)

# BEGIN WITSELF COMPONENT VERIFIER
# This sourceable verifier is dark. It never discovers roots or fetches a verifier.
function Initialize-WSComponentVerifier {
    if ('WSB2Verifier' -as [type]) { return }
    try {
        Add-Type -TypeDefinition @'
using System;
using System.IO;
using System.Text;
using System.Text.RegularExpressions;
using System.Collections.Generic;
using System.Globalization;
using System.Security.Cryptography;

public sealed class WSB2Verifier {
    sealed class Refusal : Exception { public Refusal(string code) : base(code) {} }
    sealed class NumberToken { public readonly string Text; public NumberToken(string text) { Text=text; } }
    static void Need(bool ok, string code) { if (!ok) throw new Refusal(code); }
    static readonly UTF8Encoding Utf8 = new UTF8Encoding(false, true);
    static readonly string[] Common = {"format_version","critical","kind","issued_at","expires_at"};
    static readonly string[] Roles = {"app","service_host","server","postgresql","worker","extension"};
    readonly HashSet<string> retiredRoots = new HashSet<string>(StringComparer.Ordinal);
    readonly Dictionary<string,RSA> roots = new Dictionary<string,RSA>(StringComparer.Ordinal);
    Dictionary<string,RSA> keys = new Dictionary<string,RSA>(StringComparer.Ordinal);
    HashSet<string> revoked = new HashSet<string>(StringComparer.Ordinal);
    string rootID, delegationIdentity, releaseIdentity;
    Dictionary<string,object> frozenWindowsArchive, exportedWindowsArchive;
    ulong rootSequence = 1, delegationSequence, releaseSequence;
    DateTime delegationIssued, delegationExpires;
    HashSet<ulong> rollbackTargets = new HashSet<ulong>();
    public Dictionary<string,object> Accepted { get; private set; }

    // A bounded recursive pre-pass/parser preserves case and rejects duplicate
    // names after JSON escape decoding; ConvertFrom-Json cannot do this safely.
    sealed class Json {
        readonly string s; int p;
        public Json(byte[] b) { try { s = Utf8.GetString(b); } catch { throw new Refusal("profile_violation"); } }
        void WS() { while(p < s.Length && " \t\r\n".IndexOf(s[p]) >= 0) p++; }
        char Take() { Need(p < s.Length,"invalid_json"); return s[p++]; }
        void Char(char c) { WS(); Need(Take() == c,"invalid_json"); }
        string String() {
            Need(Take() == '"',"invalid_json"); var b = new StringBuilder(); bool end = false;
            while(p < s.Length) {
                char c = Take(); if(c == '"') { end = true; break; }
                Need(c >= 32,"invalid_json");
                if(c == '\\') {
                    char e = Take();
                    if(e == 'u') { Need(p+4 <= s.Length,"invalid_json"); int n; Need(Int32.TryParse(s.Substring(p,4),NumberStyles.AllowHexSpecifier,CultureInfo.InvariantCulture,out n),"invalid_json"); c=(char)n; p+=4; }
                    else { int i = "\"\\/bfnrt".IndexOf(e); Need(i >= 0,"invalid_json"); c = "\"\\/\b\f\n\r\t"[i]; }
                }
                b.Append(c);
            }
            Need(end,"invalid_json"); string result=b.ToString();
            try { Need(Utf8.GetByteCount(result)<=4096,"profile_violation"); } catch(EncoderFallbackException) { throw new Refusal("profile_violation"); }
            return result;
        }
        object Value(int depth) {
            WS(); Need(p < s.Length,"invalid_json"); char c=s[p];
            if(c=='{' || c=='[') Need(depth<=8,"profile_violation");
            if(c=='{') {
                p++; var d=new Dictionary<string,object>(StringComparer.Ordinal); WS();
                if(p<s.Length && s[p]=='}') {p++; return d;}
                while(true) { WS(); string k=String(); Need(!d.ContainsKey(k),"duplicate_key"); Char(':'); d.Add(k,Value(depth+1)); WS(); char z=Take(); if(z=='}') return d; Need(z==',',"invalid_json"); }
            }
            if(c=='[') {
                p++; var a=new List<object>(); WS(); if(p<s.Length && s[p]==']') {p++; return a;}
                while(true) {a.Add(Value(depth+1)); WS(); char z=Take(); if(z==']') return a; Need(z==',',"invalid_json");}
            }
            if(c=='"') return String();
            foreach(string literal in new[]{"true","false","null"}) {
                if(p+literal.Length<=s.Length && s.Substring(p,literal.Length)==literal) {p+=literal.Length; if(literal=="null") return null; return literal=="true";}
            }
            int start=p; while(p<s.Length && " \t\r\n,]}".IndexOf(s[p])<0) p++;
            string num=s.Substring(start,p-start); ulong u;
            Need(Regex.IsMatch(num,@"\A-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?\z"),"invalid_json");
            if(Regex.IsMatch(num,@"\A(0|[1-9][0-9]*)\z") && UInt64.TryParse(num,NumberStyles.None,CultureInfo.InvariantCulture,out u)) return u;
            // Frozen B1 accepts JSON numbers in ignored unknown fields; uint64
            // schema fields enforce the numeric profile when selected below.
            return new NumberToken(num);
        }
        public Dictionary<string,object> Parse() { object v=Value(1); WS(); Need(p==s.Length,"invalid_json"); return Obj(v); }
    }
    static Dictionary<string,object> Obj(object o) { var d=o as Dictionary<string,object>; Need(d!=null,"invalid_document"); return d; }
    static List<object> Arr(object o) { var a=o as List<object>; Need(a!=null,"invalid_document"); return a; }
    static object Get(Dictionary<string,object> d,string k) { Need(d.ContainsKey(k),"invalid_document"); return d[k]; }
    static string Str(object o) { Need(o is string,"invalid_document"); return (string)o; }
    static string S(Dictionary<string,object> d,string k) { return Str(Get(d,k)); }
    static ulong N(Dictionary<string,object> d,string k) { object o=Get(d,k); Need(!(o is NumberToken),"profile_violation"); Need(o is ulong,"invalid_document"); return (ulong)o; }
    static ulong Positive(Dictionary<string,object> d,string k) { ulong v=N(d,k); Need(v>0,"sequence_invalid"); return v; }
    static void Exact(Dictionary<string,object> d,params string[] fields) { foreach(string k in fields) Get(d,k); foreach(string k in d.Keys) Need(Array.IndexOf(fields,k)>=0,"unknown_field"); }
    static bool Hex(string s,int count) { return Regex.IsMatch(s,@"\A[0-9a-f]{"+count+@"}\z"); }
    static void Digest(string s) { Need(Hex(s,64),"invalid_document"); }
    static string Hash(byte[] b) { using(var sha=SHA256.Create()) {return BitConverter.ToString(sha.ComputeHash(b)).Replace("-","").ToLowerInvariant();} }
    static byte[] B64(string s) { try {byte[] b=Convert.FromBase64String(s); Need(Convert.ToBase64String(b)==s,"invalid_encoding"); return b;} catch(FormatException) {throw new Refusal("invalid_encoding");} }
    static byte[] Read(string path,int cap,string reason) {
        try {
            Need((File.GetAttributes(path)&(FileAttributes.Directory|FileAttributes.ReparsePoint))==0,"state_io");
            using(var f=new FileStream(path,FileMode.Open,FileAccess.Read,FileShare.Read)) {
                Need(f.Length<=cap,reason); var b=new byte[(int)f.Length]; int n=0;
                while(n<b.Length) {int m=f.Read(b,n,b.Length-n); Need(m>0,"state_io"); n+=m;} Need(f.ReadByte()<0,reason); return b;
            }
        } catch(Refusal) {throw;} catch {throw new Refusal("state_io");}
    }
    static RSA Key(byte[] der) {
        try {
            RSA rsa=RSA.Create(); int read; rsa.ImportSubjectPublicKeyInfo(der,out read);
            var p=rsa.ExportParameters(false);
            Need(read==der.Length && rsa.KeySize==3072 && p.Modulus.Length==384 && (p.Modulus[0]&128)!=0 &&
                p.Exponent.Length==3 && p.Exponent[0]==1 && p.Exponent[1]==0 && p.Exponent[2]==1 &&
                Convert.ToBase64String(rsa.ExportSubjectPublicKeyInfo())==Convert.ToBase64String(der),"invalid_key"); return rsa;
        } catch(Refusal) {throw;} catch {throw new Refusal("invalid_key");}
    }
    static KeyValuePair<string,RSA> PublicKey(object o) {
        var d=Obj(o); Exact(d,"key_id","spki"); byte[] der=B64(S(d,"spki")); string id=S(d,"key_id");
        Need(Hex(id,64)&&Hash(der)==id,"invalid_key"); return new KeyValuePair<string,RSA>(id,Key(der));
    }
    public WSB2Verifier(string[] pemFiles) {
        Need(pemFiles!=null && pemFiles.Length>0,"no_trust_root");
        foreach(string path in pemFiles) {
            string pem=Utf8.GetString(Read(path,16384,"document_too_large"));
            var m=Regex.Match(pem,@"\A-----BEGIN (?<label>PUBLIC KEY|TEST-ONLY PUBLIC KEY)-----\s*(?<der>[A-Za-z0-9+/=\r\n]+)\s*-----END \k<label>-----\s*\z");
            Need(m.Success,"invalid_key"); byte[] der=B64(Regex.Replace(m.Groups["der"].Value,@"\s", ""));
            string id=Hash(der); Need(!roots.ContainsKey(id),"invalid_key"); roots.Add(id,Key(der));
        }
    }
    static void VerifySig(RSA key,byte[] doc,byte[] sig) { Need(key.VerifyData(doc,sig,HashAlgorithmName.SHA256,RSASignaturePadding.Pkcs1),"signature_invalid"); }
    static DateTime Time(string s) {
        DateTime t; Need(Regex.IsMatch(s,@"\A[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z\z") &&
            DateTime.TryParseExact(s,"yyyy-MM-dd'T'HH:mm:ss'Z'",CultureInfo.InvariantCulture,DateTimeStyles.AssumeUniversal|DateTimeStyles.AdjustToUniversal,out t),"invalid_document");
        return DateTime.ParseExact(s,"yyyy-MM-dd'T'HH:mm:ss'Z'",CultureInfo.InvariantCulture,DateTimeStyles.AssumeUniversal|DateTimeStyles.AdjustToUniversal);
    }
    static void Fresh(DateTime issued,DateTime expires,DateTime now,bool trusted) {
        Need(expires>issued,"invalid_document"); Need(trusted,"unknown_clock"); Need(now>=issued,"not_yet_valid"); Need(now<expires,"expired");
    }
    static void Path(string s,bool basename=false) {
        // Older .NET Unicode tables must not silently accept newly assigned
        // decomposable characters outside their normalization knowledge.
        for(int i=0;i<s.Length;i++) { Need(CharUnicodeInfo.GetUnicodeCategory(s,i)!=UnicodeCategory.OtherNotAssigned,"path_invalid"); if(Char.IsHighSurrogate(s[i])) i++; }
        Need(s.Length>0 && Utf8.GetByteCount(s)<=1024 && s.IndexOfAny(new[]{'\\',':','\0'})<0 && s.IsNormalized(NormalizationForm.FormC),"path_invalid");
        var parts=s.Split('/'); Need(!basename||parts.Length==1,"path_invalid"); foreach(string p in parts) Need(p!=""&&p!="."&&p!="..","path_invalid");
    }
    static HashSet<string> Strings(object o,string reason) { var set=new HashSet<string>(StringComparer.Ordinal); foreach(object v in Arr(o)) Need(set.Add(Str(v)),reason); return set; }
    static void CommonDoc(Dictionary<string,object> d,string kind,string[] fields) {
        Need(N(d,"format_version")==1,"invalid_document"); Need(S(d,"kind")==kind,"wrong_kind");
        var known=new HashSet<string>(Common,StringComparer.Ordinal); foreach(string k in fields) {known.Add(k); Get(d,k);}
        var critical=Strings(Get(d,"critical"),"unknown_critical"); foreach(string k in critical) Need(known.Contains(k),"unknown_critical");
    }
    static void Components(Dictionary<string,object> d) {
        Need(S(d,"channel")=="stable","invalid_document"); Need(Regex.IsMatch(S(d,"release_tag"),@"\Av(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\z"),"wrong_tag");
        var source=Obj(Get(d,"source")); Exact(source,"repository","commit"); Need(S(source,"repository").Length>0&&Hex(S(source,"commit"),40),"invalid_document");
        var provenance=Obj(Get(d,"provenance")); Exact(provenance,"checksums_sha256","sigstore_bundle"); Digest(S(provenance,"checksums_sha256")); Path(S(provenance,"sigstore_bundle")); Path(S(d,"license")); Path(S(d,"sbom"));
        var rollback=Obj(Get(d,"rollback")); Exact(rollback,"allowed_targets"); var targets=new HashSet<ulong>();
        foreach(object v in Arr(Get(rollback,"allowed_targets"))) Need(v is ulong && (ulong)v>0 && (ulong)v<N(d,"release_sequence") && targets.Add((ulong)v),"sequence_invalid");
        var platforms=Arr(Get(d,"platforms")); Need(platforms.Count>=1&&platforms.Count<=6,"wrong_platform"); var pairs=new HashSet<string>(StringComparer.Ordinal);
        foreach(object item in platforms) {
            var p=Obj(item); string os=S(p,"os"),arch=S(p,"arch");
            foreach(string k in p.Keys) Need(Array.IndexOf(new[]{"os","arch","min_os","components","glibc_floor"},k)>=0,"unknown_field");
            Need(Array.IndexOf(new[]{"linux","darwin","windows"},os)>=0 && Array.IndexOf(new[]{"amd64","arm64"},arch)>=0 && S(p,"min_os").Length>0 && pairs.Add(os+"/"+arch),"wrong_platform");
            if(os=="linux") Need(Regex.IsMatch(S(p,"glibc_floor"),@"\A2\.[0-9]{2}\z"),"wrong_platform"); else if(p.ContainsKey("glibc_floor")) Need(S(p,"glibc_floor")=="","wrong_platform");
            var roles=new Dictionary<string,int>(StringComparer.Ordinal); var paths=new HashSet<string>(StringComparer.Ordinal); var archives=new HashSet<string>(StringComparer.Ordinal); var deps=new Dictionary<string,HashSet<string>>(StringComparer.Ordinal);
            foreach(object comp in Arr(Get(p,"components"))) {
                var c=Obj(comp); string role=S(c,"role"); Need(Array.IndexOf(Roles,role)>=0,"roles_invalid");
                foreach(string k in new[]{"role","version","archive","inventory","depends_on","required_extensions","schema","ownership"}) Get(c,k);
                foreach(string k in c.Keys) Need(Array.IndexOf(new[]{"role","version","archive","inventory","depends_on","required_extensions","schema","ownership","postgresql"},k)>=0,"unknown_field");
                Need(S(c,"version").Length>0,"invalid_document"); if(!roles.ContainsKey(role)) {roles.Add(role,0); deps.Add(role,new HashSet<string>(StringComparer.Ordinal));} roles[role]++;
                var a=Obj(Get(c,"archive")); Exact(a,"name","size","sha256","format"); Path(S(a,"name"),true); Digest(S(a,"sha256"));
                Need(archives.Add(S(a,"name"))&&N(a,"size")>0&&N(a,"size")<=2147483648UL,"inventory_invalid"); Need(S(a,"format")=="zip"||S(a,"format")=="tar.gz","invalid_document");
                var entries=Arr(Get(c,"inventory")); Need(entries.Count>0&&entries.Count<=4096,"inventory_invalid"); var own=new HashSet<string>(StringComparer.Ordinal); ulong sum=0;
                foreach(object entry in entries) {
                    var e=Obj(entry); Exact(e,"path","size","sha256","mode","executable"); string path=S(e,"path"); Path(path); Digest(S(e,"sha256")); Need(paths.Add(path),"inventory_invalid"); own.Add(path);
                    ulong size=N(e,"size"); Need(size<=N(a,"size")-sum,"inventory_invalid"); sum+=size;
                    Need((S(e,"mode")=="0644"||S(e,"mode")=="0755") && Get(e,"executable") is bool && (bool)Get(e,"executable")== (S(e,"mode")=="0755"),"mode_invalid");
                }
                var ownership=Obj(Get(c,"ownership")); Exact(ownership,"package_owned_paths","private_payload_paths"); var owned=new HashSet<string>(StringComparer.Ordinal);
                foreach(string category in new[]{"package_owned_paths","private_payload_paths"}) foreach(object v in Arr(Get(ownership,category))) {string path=Str(v); Path(path); Need(owned.Add(path)&&own.Contains(path),"inventory_invalid");}
                Need(owned.SetEquals(own),"inventory_invalid");
                var schema=Obj(Get(c,"schema")); Exact(schema,"target","accepted_min","accepted_max"); Need(N(schema,"accepted_min")<=N(schema,"target")&&N(schema,"target")<=N(schema,"accepted_max"),"schema_invalid");
                Need(Arr(Get(c,"required_extensions")).Count==0,"extensions_unsupported");
                foreach(string dep in Strings(Get(c,"depends_on"),"roles_invalid")) {Need(dep!=role&&Array.IndexOf(Roles,dep)>=0,"roles_invalid"); deps[role].Add(dep);}
                if(role=="postgresql") {
                    var pg=Obj(Get(c,"postgresql")); Exact(pg,"major","version","distribution","build","runtime_libraries","data_layout_version");
                    Need(N(pg,"major")==16&&S(pg,"version")==S(c,"version")&&S(pg,"version").StartsWith("16.",StringComparison.Ordinal)&&S(pg,"distribution").Length>0&&S(pg,"build").Length>0&&N(pg,"data_layout_version")>0,"postgresql_invalid");
                    foreach(object v in Arr(Get(pg,"runtime_libraries"))) Path(Str(v));
                } else Need(!c.ContainsKey("postgresql"),"postgresql_invalid");
            }
            foreach(string path in paths) {int slash=path.IndexOf('/'); while(slash>=0) {Need(!paths.Contains(path.Substring(0,slash)),"inventory_invalid"); slash=path.IndexOf('/',slash+1);}}
            foreach(string role in new[]{"app","service_host","server","postgresql"}) Need(roles.ContainsKey(role)&&roles[role]==1,"roles_invalid");
            Need(roles.ContainsKey("worker")&&roles["worker"]>=1&&(!roles.ContainsKey("extension")||roles["extension"]<=1),"roles_invalid");
            foreach(string role in deps.Keys) Walk(role,deps,new HashSet<string>(StringComparer.Ordinal));
        }
    }
    static void Walk(string role,Dictionary<string,HashSet<string>> deps,HashSet<string> seen) { Need(deps.ContainsKey(role)&&seen.Add(role),"roles_invalid"); foreach(string dep in deps[role]) Walk(dep,deps,new HashSet<string>(seen,StringComparer.Ordinal)); }
    public Dictionary<string,object> Verify(string document,string envelope,string kind,DateTime now,bool trusted,string tag,string os,string arch,bool allowRollback) {
        try { return VerifyInner(document,envelope,kind,now.ToUniversalTime(),trusted,tag,os,arch,allowRollback); } catch(Refusal) {throw;} catch {throw new Refusal("invalid_document");}
    }
    Dictionary<string,object> VerifyInner(string document,string envelope,string kind,DateTime now,bool trusted,string tag,string os,string arch,bool allowRollback) {
        byte[] raw=Read(document,kind=="component_set"?262144:16384,"document_too_large"), eraw=Read(envelope,8192,"envelope_too_large");
        var e=new Json(eraw).Parse(); Exact(e,"format_version","kind","algorithm","signatures"); Need(N(e,"format_version")==1,"invalid_document"); Need(S(e,"kind")==kind,"wrong_kind"); Need(S(e,"algorithm")=="rsa3072-pkcs1v15-sha256","wrong_algorithm");
        var sigs=new Dictionary<string,byte[]>(StringComparer.Ordinal);
        foreach(object item in Arr(Get(e,"signatures"))) {var sig=Obj(item); Exact(sig,"key_id","signature"); string id=S(sig,"key_id"); Digest(id); byte[] bytes=B64(S(sig,"signature")); Need(bytes.Length==384,"signature_length"); Need(!sigs.ContainsKey(id),"signature_count"); sigs.Add(id,bytes);}
        Need(sigs.Count==(kind=="root_rotation"?2:1),"signature_count"); string signer=null;
        foreach(string id in sigs.Keys) {
            if(kind=="component_set") {Need(!revoked.Contains(id),"revoked_key"); Need(!roots.ContainsKey(id),"wrong_signer"); Need(keys.ContainsKey(id),"unknown_key"); VerifySig(keys[id],raw,sigs[id]); signer=id; break;}
            if(roots.ContainsKey(id)&&!retiredRoots.Contains(id)&&(rootID==null||rootID==id)) {VerifySig(roots[id],raw,sigs[id]); signer=id; break;}
        }
        Need(signer!=null,"wrong_signer"); var d=new Json(raw).Parse();
        string[] fields=kind=="delegation"?new[]{"root_sequence","delegation_sequence","keys","revoked_key_ids"}:kind=="root_rotation"?new[]{"root_sequence","new_sequence","new_root"}:new[]{"root_sequence","delegation_sequence","release_sequence","release_tag","channel","source","provenance","license","sbom","rollback","platforms"};
        CommonDoc(d,kind,fields); DateTime issued=Time(S(d,"issued_at")),expires=Time(S(d,"expires_at")); Fresh(issued,expires,now,trusted);
        Need(Positive(d,"root_sequence")==rootSequence,"sequence_invalid"); string identity=Hash(raw);
        if(kind=="root_rotation") {
            Need(rootSequence<UInt64.MaxValue&&Positive(d,"new_sequence")==rootSequence+1,"sequence_invalid"); var next=PublicKey(Get(d,"new_root")); Need(!retiredRoots.Contains(next.Key),"invalid_key");
            if(signer==next.Key) { signer=null; foreach(string id in sigs.Keys) if(id!=next.Key&&roots.ContainsKey(id)&&(rootID==null||rootID==id)&&!retiredRoots.Contains(id)) { VerifySig(roots[id],raw,sigs[id]); signer=id; break; } }
            Need(signer!=null&&signer!=next.Key,"wrong_signer"); Need(sigs.ContainsKey(next.Key),"wrong_signer"); VerifySig(next.Value,raw,sigs[next.Key]);
            retiredRoots.Add(signer); if(!roots.ContainsKey(next.Key)) roots.Add(next.Key,next.Value); rootID=next.Key; rootSequence++; delegationSequence=0; delegationIdentity=null; keys.Clear(); revoked.Clear();
        } else if(kind=="delegation") {
            ulong sequence=Positive(d,"delegation_sequence"); Need(sequence>=delegationSequence,"replay"); Need(sequence!=delegationSequence||identity==delegationIdentity,"sequence_invalid");
            var next=new Dictionary<string,RSA>(StringComparer.Ordinal); foreach(object item in Arr(Get(d,"keys"))) {var key=PublicKey(item); Need(!roots.ContainsKey(key.Key)&&!next.ContainsKey(key.Key),"invalid_key"); next.Add(key.Key,key.Value);} Need(next.Count>0,"invalid_document");
            var rev=Strings(Get(d,"revoked_key_ids"),"invalid_key"); foreach(string id in rev) Digest(id);
            keys=next; revoked=rev; rootID=signer; delegationSequence=sequence; delegationIdentity=identity; delegationIssued=issued; delegationExpires=expires;
        } else {
            Need(kind=="component_set","wrong_kind"); Need(Positive(d,"delegation_sequence")==delegationSequence,"sequence_invalid"); Fresh(delegationIssued,delegationExpires,now,trusted); Need(issued>=delegationIssued&&expires<=delegationExpires,"invalid_document");
            Positive(d,"release_sequence"); Components(d); if(!String.IsNullOrEmpty(tag)) Need(S(d,"release_tag")==tag,"wrong_tag");
            if(!String.IsNullOrEmpty(os)||!String.IsNullOrEmpty(arch)) {bool found=false; foreach(object item in Arr(Get(d,"platforms"))) {var p=Obj(item); if(S(p,"os")==os&&S(p,"arch")==arch) found=true;} Need(found,"wrong_platform");}
            ulong sequence=N(d,"release_sequence"); if(sequence<releaseSequence) {Need(allowRollback,"replay"); Need(rollbackTargets.Contains(sequence),"rollback_denied");}
            Need(sequence!=releaseSequence||identity==releaseIdentity,"sequence_invalid");
            if(sequence>releaseSequence) {releaseSequence=sequence; releaseIdentity=identity; rollbackTargets.Clear(); foreach(object v in Arr(Get(Obj(Get(d,"rollback")),"allowed_targets"))) rollbackTargets.Add((ulong)v);}
            Accepted=d; frozenWindowsArchive=null; exportedWindowsArchive=null;
            foreach(object item in Arr(Get(d,"platforms"))) {var p=Obj(item); if(S(p,"os")!="windows"||S(p,"arch")!="amd64") continue; foreach(object v in Arr(Get(p,"components"))) {var c=Obj(v); if(S(c,"role")=="app") frozenWindowsArchive=new Dictionary<string,object>(Obj(Get(c,"archive")),StringComparer.Ordinal);}}
        }
        return d;
    }
    public Dictionary<string,object> App(string os,string arch) {
        Need(Accepted!=null,"not_installed"); foreach(object item in Arr(Get(Accepted,"platforms"))) {var p=Obj(item); if(S(p,"os")!=os||S(p,"arch")!=arch) continue; foreach(object v in Arr(Get(p,"components"))) {var c=Obj(v); if(S(c,"role")=="app") { Need(os=="windows"&&arch=="amd64"&&frozenWindowsArchive!=null,"wrong_platform"); exportedWindowsArchive=new Dictionary<string,object>(frozenWindowsArchive,StringComparer.Ordinal); return exportedWindowsArchive; }}} throw new Refusal("wrong_platform");
    }
    public void CheckArchive(string path,Dictionary<string,object> archive) {
        try {
            Need(Object.ReferenceEquals(exportedWindowsArchive,archive),"mixed_set");
            foreach(string k in frozenWindowsArchive.Keys) Need(archive.ContainsKey(k)&&Object.Equals(archive[k],frozenWindowsArchive[k]),"mixed_set");
            Need((File.GetAttributes(path)&(FileAttributes.Directory|FileAttributes.ReparsePoint))==0,"filesystem_error");
            ulong expected=N(frozenWindowsArchive,"size"),total=0;
            using(var f=new FileStream(path,FileMode.Open,FileAccess.Read,FileShare.Read)) using(var sha=SHA256.Create()) {
                Need(f.Length==(long)expected,"archive_size_mismatch"); byte[] buffer=new byte[32768];
                while(true) {
                    int count=f.Read(buffer,0,(int)Math.Min((ulong)buffer.Length,expected-total+1));
                    if(count==0) break; total+=(ulong)count; Need(total<=expected,"archive_size_mismatch");
                    sha.TransformBlock(buffer,0,count,buffer,0);
                }
                Need(total==expected,"archive_size_mismatch"); sha.TransformFinalBlock(new byte[0],0,0);
                Need(BitConverter.ToString(sha.Hash).Replace("-","").ToLowerInvariant()==S(frozenWindowsArchive,"sha256"),"archive_digest_mismatch");
            }
        } catch(Refusal) {throw;} catch {throw new Refusal("filesystem_error");}
    }
}
'@ -ErrorAction Stop
    } catch { throw 'invalid_key: trusted PowerShell 7 with .NET RSA SubjectPublicKeyInfo support is required' }
}

function Get-WSComponentRefusal {
    param($Failure)
    $exception = $Failure.Exception
    while ($null -ne $exception.InnerException) { $exception = $exception.InnerException }
    $reason = $exception.Message
    if ($reason -ceq 'invalid_key: trusted PowerShell 7 with .NET RSA SubjectPublicKeyInfo support is required') { return 'invalid_key' }
    $closedReasons = @(
        'no_trust_root','document_too_large','envelope_too_large','invalid_json','duplicate_key',
        'profile_violation','unknown_critical','unknown_field','invalid_document','wrong_kind',
        'wrong_algorithm','invalid_key','invalid_encoding','signature_length','signature_count',
        'signature_invalid','unknown_key','revoked_key','wrong_signer','state_invalid','state_io',
        'sequence_invalid','replay','rollback_denied','wrong_tag','wrong_platform','schema_invalid',
        'postgresql_invalid','extensions_unsupported','roles_invalid','path_invalid','inventory_invalid',
        'mode_invalid','not_installed','mixed_set','expired','not_yet_valid','unknown_clock',
        'invalid_inventory','unsafe_path','unsupported_archive','archive_size_mismatch',
        'archive_digest_mismatch','archive_invalid','archive_entry_type','archive_duplicate_entry',
        'archive_undeclared_entry','archive_missing_entry','entry_size_mismatch','entry_digest_mismatch',
        'entry_mode_mismatch','inventory_limit','staging_not_fresh','filesystem_error','cancelled'
    )
    if ($closedReasons -cnotcontains $reason) { return 'invalid_document' }
    return $reason
}

function Get-WSVerifiedComponentSet {
    param([string]$Directory, [string]$RootPEM, [string]$ExpectedTag)
    if ([string]::IsNullOrWhiteSpace($RootPEM)) { throw 'no_trust_root' }
    Initialize-WSComponentVerifier
    try {
        $verifier = New-Object WSB2Verifier -ArgumentList (,[string[]]@($RootPEM))
        $now = [DateTime]::UtcNow
        $trusted = $env:WS_UNTRUSTED_CLOCK -ne '1'
        $rotations = @(Get-ChildItem -LiteralPath $Directory -Filter 'root-rotation-*.json' | Where-Object { $_.Name -notlike '*.sig.json' })
        if ($rotations.Count -gt 32) { throw 'sequence_invalid' }
        foreach ($rotation in $rotations) {
            if ($rotation.Name -cnotmatch '^root-rotation-[1-9][0-9]*\.json$') { throw 'sequence_invalid' }
            $rotationNumber = 0
            if (-not [int]::TryParse(([regex]::Match($rotation.Name, '[0-9]+').Value), [ref]$rotationNumber) -or $rotationNumber -gt 32) { throw 'sequence_invalid' }
        }
        $sequence = 1
        foreach ($rotation in @($rotations | Sort-Object { [int]([regex]::Match($_.Name, '^root-rotation-([0-9]+)\.json$').Groups[1].Value) })) {
            if ($rotation.Name -cne "root-rotation-$sequence.json") { throw 'sequence_invalid' }
            $null = $verifier.Verify($rotation.FullName, "$($rotation.FullName).sig.json", 'root_rotation', $now, $trusted, '', '', '', $false)
            $sequence++
        }
        $delegation = Join-Path $Directory 'delegation.json'
        $null = $verifier.Verify($delegation, "$delegation.sig.json", 'delegation', $now, $trusted, '', '', '', $false)
        $manifest = Join-Path $Directory 'component-set.json'
        $null = $verifier.Verify($manifest, "$manifest.sig.json", 'component_set', $now, $trusted, $ExpectedTag, 'windows', 'amd64', $false)
        return $verifier
    } catch { throw (Get-WSComponentRefusal $_) }
}
# END WITSELF COMPONENT VERIFIER

if ($VerifierOnly) { return }

& {
param(
    [string]$Version,
    [string]$InstallDir,
    [bool]$NoPathUpdate,
    [string]$ReleaseRoot
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

function Test-WebSource {
    param([string]$Value)
    return $Value -match '^https://'
}

function Copy-ReleaseItem {
    param(
        [string]$Source,
        [string]$Destination
    )
    if (Test-WebSource $Source) {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        Invoke-WebRequest `
            -UseBasicParsing `
            -Headers @{ 'User-Agent' = 'witself-install.ps1' } `
            -Uri $Source `
            -OutFile $Destination
        return
    }
    if (-not (Test-Path -LiteralPath $Source -PathType Leaf)) {
        throw "release item not found: $Source"
    }
    Copy-Item -LiteralPath $Source -Destination $Destination -Force
}

function Get-ReleaseItemSource {
    param(
        [string]$Root,
        [string]$Tag,
        [string]$Name
    )
    if (Test-WebSource $Root) {
        return "$($Root.TrimEnd('/'))/$Tag/$Name"
    }
    return Join-Path (Join-Path ([IO.Path]::GetFullPath($Root)) $Tag) $Name
}

function Get-LatestVersion {
    param([string]$Root)
    if (-not (Test-WebSource $Root)) {
        $latestPath = Join-Path ([IO.Path]::GetFullPath($Root)) 'latest.json'
        if (-not (Test-Path -LiteralPath $latestPath -PathType Leaf)) {
            throw "latest release metadata not found: $latestPath"
        }
        $latest = Get-Content -LiteralPath $latestPath -Raw | ConvertFrom-Json
        return [string]$latest.tag_name
    }
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    $latest = Invoke-RestMethod `
        -UseBasicParsing `
        -Headers @{ 'User-Agent' = 'witself-install.ps1' } `
        -Uri 'https://api.github.com/repos/witwave-ai/witself/releases/latest'
    return [string]$latest.tag_name
}

function Assert-WitselfVersionOutput {
    param(
        [object[]]$Output,
        [string]$ExpectedVersion,
        [string]$Label
    )
    if ($Output.Count -ne 1) {
        throw "$Label reported a version other than requested v$ExpectedVersion"
    }
    $actual = [string]$Output[0]
    $escapedVersion = [regex]::Escape($ExpectedVersion)
    if ($actual -notmatch "^witself $escapedVersion(?: \(commit [^`r`n]+, built [^`r`n]+\))?$") {
        throw "$Label reported a version other than requested v$ExpectedVersion"
    }
}

function Assert-ReplaceableTarget {
    param([string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) {
        return
    }
    $item = Get-Item -LiteralPath $Path -Force
    if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
        throw "refusing to replace non-regular install target: $Path"
    }
}

function Assert-WitselfReleaseArchive {
    param([string]$Path)
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $archive = [IO.Compression.ZipFile]::OpenRead($Path)
    try {
        $entries = @($archive.Entries)
        if ($entries.Count -ne 1) {
            throw 'release archive must contain exactly one regular root entry named witself.exe'
        }
        $entry = $entries[0]
        # ExternalAttributes is signed on older PowerShell/.NET combinations.
        # Normalize it to an unsigned 32-bit value before inspecting both the
        # Unix mode in the upper word and the DOS directory bit below.
        $externalAttributes = ([int64]$entry.ExternalAttributes -band 4294967295)
        $unixFileType = (($externalAttributes -shr 16) -band 0xF000)
        $isDosDirectory = (($externalAttributes -band 0x10) -ne 0)
        if ($entry.FullName -cne 'witself.exe' -or
            $entry.Name -cne 'witself.exe' -or
            ($unixFileType -ne 0 -and $unixFileType -ne 0x8000) -or
            $isDosDirectory -or
            $entry.Length -gt 268435456) {
            throw 'release archive must contain exactly one regular root entry named witself.exe'
        }
    } finally {
        $archive.Dispose()
    }
}

function Open-InstallDirectoryLock {
    param([string]$Path)
    $timeoutMilliseconds = 30000
    if (-not [string]::IsNullOrWhiteSpace($env:WS_INSTALL_LOCK_TIMEOUT_MS)) {
        $parsedTimeout = 0
        if (-not [int]::TryParse($env:WS_INSTALL_LOCK_TIMEOUT_MS, [ref]$parsedTimeout) -or
            $parsedTimeout -lt 0 -or $parsedTimeout -gt 300000) {
            throw 'WS_INSTALL_LOCK_TIMEOUT_MS must be an integer from 0 through 300000'
        }
        $timeoutMilliseconds = $parsedTimeout
    }
    $deadline = [DateTime]::UtcNow.AddMilliseconds($timeoutMilliseconds)
    while ($true) {
        try {
            return [IO.File]::Open(
                $Path,
                [IO.FileMode]::OpenOrCreate,
                [IO.FileAccess]::ReadWrite,
                [IO.FileShare]::None
            )
        } catch [IO.IOException] {
            if ([DateTime]::UtcNow -ge $deadline) {
                throw "timed out waiting for another Witself installer to release $Path"
            }
            Start-Sleep -Milliseconds 100
        }
    }
}

# Defender and other on-access scanners briefly hold freshly written
# executables open without delete sharing, so File.Replace and File.Move can
# fail with an in-use error that clears within a second. Retry only that
# signature, and only after verifying that the failed call left the untouched
# pre-mutation layout. Any other failure, and any other layout (for example
# the documented 1177 partial-replace state), must surface the original error
# unmodified so Restore-InstalledFile can reason about recovery.
function Invoke-StagedFileMutation {
    param(
        [string]$Stage,
        [string]$Target,
        [string]$Backup
    )
    $retryDelaysMilliseconds = @(250, 500, 1000)
    for ($attempt = 0; ; $attempt++) {
        try {
            if ([string]::IsNullOrEmpty($Backup)) {
                [IO.File]::Move($Stage, $Target)
            } else {
                [IO.File]::Replace($Stage, $Target, $Backup, $true)
            }
            return
        } catch [IO.IOException] {
            if ($attempt -ge $retryDelaysMilliseconds.Count) {
                throw
            }
            # ERROR_SHARING_VIOLATION, ERROR_LOCK_VIOLATION, and ReplaceFile's
            # ERROR_UNABLE_TO_REMOVE_REPLACED and ERROR_UNABLE_TO_MOVE_REPLACEMENT
            # report a hold that left both files under their original names.
            $transientInUse = @(0x80070020, 0x80070021, 0x80070497, 0x80070498)
            if ($transientInUse -notcontains $_.Exception.GetBaseException().HResult) {
                throw
            }
            $untouched = if ([string]::IsNullOrEmpty($Backup)) {
                (Test-Path -LiteralPath $Stage -PathType Leaf) -and
                    -not (Test-Path -LiteralPath $Target)
            } else {
                (Test-Path -LiteralPath $Stage -PathType Leaf) -and
                    (Test-Path -LiteralPath $Target -PathType Leaf) -and
                    -not (Test-Path -LiteralPath $Backup)
            }
            if (-not $untouched) {
                throw
            }
            Start-Sleep -Milliseconds $retryDelaysMilliseconds[$attempt]
        }
    }
}

function Install-StagedFile {
    param(
        [string]$Stage,
        [string]$Target,
        [string]$Backup,
        [ref]$HadOriginal,
        [ref]$Changed,
        [ref]$OriginalHash
    )
    Assert-ReplaceableTarget $Target
    if (Test-Path -LiteralPath $Target -PathType Leaf) {
        $HadOriginal.Value = $true
        $OriginalHash.Value = (Get-FileHash -LiteralPath $Target -Algorithm SHA256).Hash.ToLowerInvariant()
        $Changed.Value = $true
        Invoke-StagedFileMutation $Stage $Target $Backup
    } else {
        Invoke-StagedFileMutation $Stage $Target ''
        $Changed.Value = $true
    }
}

function Restore-InstalledFile {
    param(
        [string]$Target,
        [string]$Backup,
        [bool]$HadOriginal,
        [bool]$Changed,
        [string]$ExpectedInstalledHash,
        [string]$ExpectedOriginalHash,
        [string]$ExpectedBackupHash,
        [string]$TargetQuarantine,
        [string]$BackupQuarantine
    )
    if (-not $Changed) {
        return
    }

    # File.Replace can fail without changing either input. In that state no
    # backup exists and there is nothing to roll back. The comparison is
    # read-only, so a later writer is left untouched rather than deleted.
    if ($HadOriginal -and -not (Test-Path -LiteralPath $Backup -PathType Leaf)) {
        if (-not (Test-Path -LiteralPath $Target -PathType Leaf)) {
            throw "replacement state is uncertain: the original target and backup are both missing ($Target, $Backup)"
        }
        $item = Get-Item -LiteralPath $Target -Force
        if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
            throw "refusing to inspect a target changed to a non-regular file: $Target"
        }
        $actualTargetHash = (Get-FileHash -LiteralPath $Target -Algorithm SHA256).Hash.ToLowerInvariant()
        if (-not [string]::IsNullOrWhiteSpace($ExpectedOriginalHash) -and
            $actualTargetHash -eq $ExpectedOriginalHash) {
            return
        }
        throw "replacement state is uncertain; the current target was left untouched and no recovery backup exists: $Target"
    }

    $targetQuarantined = $false
    if (Test-Path -LiteralPath $Target) {
        $item = Get-Item -LiteralPath $Target -Force
        if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
            throw "refusing to restore over a target changed to a non-regular file: $Target"
        }
        if (Test-Path -LiteralPath $TargetQuarantine) {
            throw "rollback quarantine already exists: $TargetQuarantine"
        }
        # File.Move is a same-directory, no-overwrite rename. It captures the
        # exact mutation-time target before any hash decision, so a later writer
        # can only win the now-vacant name; it is never overwritten by rollback.
        [IO.File]::Move($Target, $TargetQuarantine)
        $targetQuarantined = $true
        $actualInstalledHash = (Get-FileHash -LiteralPath $TargetQuarantine -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actualInstalledHash -ne $ExpectedInstalledHash) {
            try {
                [IO.File]::Move($TargetQuarantine, $Target)
            } catch {
                throw "concurrent target was preserved at $TargetQuarantine because its original name could not be restored: $($_.Exception.Message)"
            }
            throw "refusing to replace a concurrently changed install target; prior recovery backup remains at $Backup"
        }
    }
    if (-not $HadOriginal) {
        if ($targetQuarantined) {
            Remove-Item -LiteralPath $TargetQuarantine -Force
        }
        return
    }
    $backupQuarantined = $false
    try {
        if (-not (Test-Path -LiteralPath $Backup -PathType Leaf)) {
            throw "backup needed for restoration is missing: $Backup"
        }
        if (Test-Path -LiteralPath $BackupQuarantine) {
            throw "backup quarantine already exists: $BackupQuarantine"
        }
        [IO.File]::Move($Backup, $BackupQuarantine)
        $backupQuarantined = $true
        $actualBackupHash = (Get-FileHash -LiteralPath $BackupQuarantine -Algorithm SHA256).Hash.ToLowerInvariant()
        if ([string]::IsNullOrWhiteSpace($ExpectedBackupHash) -or $actualBackupHash -ne $ExpectedBackupHash) {
            throw "backup changed before restoration: $BackupQuarantine"
        }
        [IO.File]::Move($BackupQuarantine, $Target)
        $backupQuarantined = $false
        if ($targetQuarantined) {
            Remove-Item -LiteralPath $TargetQuarantine -Force
        }
    } catch {
        $restoreFailure = $_.Exception.Message
        $recoveryErrors = New-Object System.Collections.Generic.List[string]
        if (-not (Test-Path -LiteralPath $Target) -and
            (Test-Path -LiteralPath $TargetQuarantine -PathType Leaf)) {
            try {
                [IO.File]::Move($TargetQuarantine, $Target)
            } catch {
                $recoveryErrors.Add("restore rejected target from ${TargetQuarantine}: $($_.Exception.Message)")
            }
        }
        if ($backupQuarantined -and -not (Test-Path -LiteralPath $Backup) -and
            (Test-Path -LiteralPath $BackupQuarantine -PathType Leaf)) {
            try {
                [IO.File]::Move($BackupQuarantine, $Backup)
            } catch {
                $recoveryErrors.Add("restore prior backup from ${BackupQuarantine}: $($_.Exception.Message)")
            }
        }
        if ($recoveryErrors.Count -gt 0) {
            throw "$restoreFailure; rollback recovery was incomplete: $([string]::Join('; ', $recoveryErrors))"
        }
        throw $restoreFailure
    }
}

function Normalize-PathEntry {
    param([string]$Value)
    if ([string]::IsNullOrWhiteSpace($Value)) {
        return ''
    }
    try {
        return ([IO.Path]::GetFullPath($Value)).TrimEnd([char[]]@('\', '/'))
    } catch {
        return $Value.Trim().TrimEnd([char[]]@('\', '/'))
    }
}

function Open-UserPathLock {
    $localAppData = [Environment]::GetFolderPath([Environment+SpecialFolder]::LocalApplicationData)
    if ([string]::IsNullOrWhiteSpace($localAppData)) {
        throw 'could not resolve LocalApplicationData for the user PATH lock'
    }
    $lockDirectory = Join-Path $localAppData 'Witself\locks'
    New-Item -ItemType Directory -Path $lockDirectory -Force | Out-Null
    $lockPath = Join-Path $lockDirectory 'user-path.lock'
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    while ($true) {
        try {
            return [IO.File]::Open(
                $lockPath,
                [IO.FileMode]::OpenOrCreate,
                [IO.FileAccess]::ReadWrite,
                [IO.FileShare]::None
            )
        } catch [IO.IOException] {
            if ([DateTime]::UtcNow -ge $deadline) {
                throw "timed out waiting to update the user PATH ($lockPath)"
            }
            Start-Sleep -Milliseconds 100
        }
    }
}

function Get-RawUserPathState {
    param([Microsoft.Win32.RegistryKey]$Key)
    $exists = @($Key.GetValueNames()) -contains 'Path'
    if (-not $exists) {
        return @{ Exists = $false; Value = ''; Kind = [Microsoft.Win32.RegistryValueKind]::ExpandString }
    }
    $kind = $Key.GetValueKind('Path')
    if ($kind -ne [Microsoft.Win32.RegistryValueKind]::String -and
        $kind -ne [Microsoft.Win32.RegistryValueKind]::ExpandString) {
        throw "refusing to update the user Path registry value because its type is $kind"
    }
    $value = $Key.GetValue(
        'Path',
        '',
        [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames
    )
    return @{ Exists = $true; Value = [string]$value; Kind = $kind }
}

function Ensure-UserPath {
    param([string]$Directory)
    $normalized = Normalize-PathEntry $Directory
    # The HKCU Path value is shared across login sessions. A per-user file lock
    # under LocalAppData coordinates console, RDP, and scheduled-task installers
    # without relying on session-scoped named kernel objects.
    $pathLock = Open-UserPathLock
    try {
        $environmentKey = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
        if ($null -eq $environmentKey) {
            throw 'could not open HKCU\Environment for user PATH update'
        }
        try {
            # Read the raw registry text under the cross-session lock. Using
            # DoNotExpandEnvironmentNames preserves unrelated %VAR% entries and
            # SetValue receives the exact pre-existing REG_SZ/REG_EXPAND_SZ kind.
            $pathState = Get-RawUserPathState $environmentKey
            $userPath = [string]$pathState.Value
            $entries = @()
            if (-not [string]::IsNullOrWhiteSpace($userPath)) {
                $entries = @($userPath.Split(';') | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
            }
            $present = $false
            foreach ($entry in $entries) {
                if ([string]::Equals((Normalize-PathEntry $entry), $normalized, [StringComparison]::OrdinalIgnoreCase)) {
                    $present = $true
                    break
                }
            }
            if (-not $present) {
                $updated = if ([string]::IsNullOrWhiteSpace($userPath)) { $Directory } else { "$userPath;$Directory" }
                $environmentKey.SetValue('Path', $updated, $pathState.Kind)
                Write-Host "Added $Directory to the user PATH."
            }
        } finally {
            $environmentKey.Dispose()
        }
    } finally {
        $pathLock.Dispose()
    }

    $processEntries = @()
    if (-not [string]::IsNullOrWhiteSpace($env:Path)) {
        $processEntries = @($env:Path.Split(';') | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
    }
    $processPresent = $false
    foreach ($entry in $processEntries) {
        if ([string]::Equals((Normalize-PathEntry $entry), $normalized, [StringComparison]::OrdinalIgnoreCase)) {
            $processPresent = $true
            break
        }
    }
    if (-not $processPresent) {
        $env:Path = if ([string]::IsNullOrWhiteSpace($env:Path)) { $Directory } else { "$($env:Path);$Directory" }
    }
}

try {
    $architecture = if (-not [string]::IsNullOrWhiteSpace($env:PROCESSOR_ARCHITEW6432)) {
        $env:PROCESSOR_ARCHITEW6432
    } else {
        $env:PROCESSOR_ARCHITECTURE
    }
    if ($architecture -ne 'AMD64') {
        throw "unsupported Windows architecture '$architecture' (windows/amd64 only)"
    }

    if ([string]::IsNullOrWhiteSpace($ReleaseRoot)) {
        $ReleaseRoot = 'https://github.com/witwave-ai/witself/releases/download'
    }
    if ($ReleaseRoot -match '^[A-Za-z][A-Za-z0-9+.-]*://' -and
        -not (Test-WebSource $ReleaseRoot)) {
        throw 'remote release roots must use HTTPS'
    }
    if ([string]::IsNullOrWhiteSpace($InstallDir)) {
        $localAppData = [Environment]::GetFolderPath([Environment+SpecialFolder]::LocalApplicationData)
        if ([string]::IsNullOrWhiteSpace($localAppData)) {
            throw 'could not resolve the per-user LocalApplicationData directory'
        }
        $InstallDir = Join-Path $localAppData 'Witself\bin'
    }
    $InstallDir = [IO.Path]::GetFullPath($InstallDir)

    $componentVerifier = $null
    $componentArchive = $null
    if (-not [string]::IsNullOrWhiteSpace($env:WS_COMPONENT_SET_DIR)) {
        # An explicitly selected component bundle emits fixed outcomes only:
        # route every later progress line of this install to a sink.
        function Hide-WSHostLine { $null = $args }
        Set-Alias -Name Write-Host -Value Hide-WSHostLine
        $expectedTag = if ([string]::IsNullOrWhiteSpace($Version)) { '' } else { 'v' + $Version.TrimStart('v') }
        $componentVerifier = Get-WSVerifiedComponentSet $env:WS_COMPONENT_SET_DIR $env:WS_TRUST_ROOT_PEM_FILE $expectedTag
        $componentArchive = $componentVerifier.App('windows', 'amd64')
        if ($componentArchive['format'] -cne 'zip') { throw 'unsupported_archive' }
        $Version = [string]$componentVerifier.Accepted['release_tag']
    }

    if ([string]::IsNullOrWhiteSpace($Version)) {
        Write-Host 'Resolving latest Witself release...'
        $Version = Get-LatestVersion $ReleaseRoot
    }
    $Version = $Version.Trim()
    $versionMatch = [regex]::Match(
        $Version,
        '^v?([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)$'
    )
    if (-not $versionMatch.Success) {
        throw "invalid release version '$Version'"
    }
    $versionWithoutPrefix = $versionMatch.Groups[1].Value
    $tag = "v$versionWithoutPrefix"
    $asset = "witself_${versionWithoutPrefix}_windows_amd64.zip"
    if ($null -ne $componentArchive) { $asset = [string]$componentArchive['name'] }

    Write-Host "Installing Witself $tag (windows/amd64)..."
    $temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) ("witself-install-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $temporaryRoot | Out-Null
    try {
        $archivePath = Join-Path $temporaryRoot $asset
        $checksumsPath = Join-Path $temporaryRoot 'checksums.txt'
        Copy-ReleaseItem (Get-ReleaseItemSource $ReleaseRoot $tag $asset) $archivePath
        if ($null -ne $componentVerifier) {
            try { $componentVerifier.CheckArchive($archivePath, $componentArchive) } catch { throw (Get-WSComponentRefusal $_) }
        } else {
        Copy-ReleaseItem (Get-ReleaseItemSource $ReleaseRoot $tag 'checksums.txt') $checksumsPath

        $expected = $null
        foreach ($line in Get-Content -LiteralPath $checksumsPath) {
            $match = [regex]::Match($line, '^\s*([0-9A-Fa-f]{64})\s+\*?(.+?)\s*$')
            if ($match.Success -and $match.Groups[2].Value -eq $asset) {
                if ($null -ne $expected) {
                    throw "duplicate checksum entries for $asset"
                }
                $expected = $match.Groups[1].Value.ToLowerInvariant()
            }
        }
        if ($null -eq $expected) {
            throw "no checksum found for $asset"
        }
        $actual = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actual -ne $expected) {
            throw "checksum mismatch for $asset (expected $expected, got $actual)"
        }
        Write-Host 'Checksum verified.'
        }

        Assert-WitselfReleaseArchive $archivePath
        $extractRoot = Join-Path $temporaryRoot 'extract'
        Expand-Archive -LiteralPath $archivePath -DestinationPath $extractRoot -Force
        $extracted = Join-Path $extractRoot 'witself.exe'
        if (-not (Test-Path -LiteralPath $extracted -PathType Leaf)) {
            throw 'witself.exe was not found at the root of the release archive'
        }
        $expectedInstalledHash = (Get-FileHash -LiteralPath $extracted -Algorithm SHA256).Hash.ToLowerInvariant()

        New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
        $installLockPath = Join-Path $InstallDir '.witself-install.lock'
        $installLock = Open-InstallDirectoryLock $installLockPath
        try {
        $destination = Join-Path $InstallDir 'witself.exe'
        $alias = Join-Path $InstallDir 'ws.exe'
        Assert-ReplaceableTarget $destination
        Assert-ReplaceableTarget $alias

        $transaction = [Guid]::NewGuid().ToString('N')
        # Keep the native executable suffix last. Windows PowerShell does not
        # reliably classify a PE file with a transaction suffix as a native
        # command, so a successful staged self-test may leave LASTEXITCODE
        # undefined under Set-StrictMode.
        $primaryStage = Join-Path $InstallDir (".witself.stage.$transaction.exe")
        $aliasStage = Join-Path $InstallDir (".ws.stage.$transaction.exe")
        $primaryBackup = Join-Path $InstallDir (".witself.exe.backup.$transaction")
        $aliasBackup = Join-Path $InstallDir (".ws.exe.backup.$transaction")
        $primaryTargetQuarantine = Join-Path $InstallDir (".witself.exe.rollback-target.$transaction")
        $aliasTargetQuarantine = Join-Path $InstallDir (".ws.exe.rollback-target.$transaction")
        $primaryBackupQuarantine = Join-Path $InstallDir (".witself.exe.rollback-backup.$transaction")
        $aliasBackupQuarantine = Join-Path $InstallDir (".ws.exe.rollback-backup.$transaction")
        Copy-Item -LiteralPath $extracted -Destination $primaryStage
        Copy-Item -LiteralPath $extracted -Destination $aliasStage
        $hadPrimary = Test-Path -LiteralPath $destination -PathType Leaf
        $hadAlias = Test-Path -LiteralPath $alias -PathType Leaf
        $primaryOriginalHash = $null
        $aliasOriginalHash = $null
        $primaryBackupHash = $null
        $aliasBackupHash = $null
        $primaryChanged = $false
        $aliasChanged = $false
        $preserveBackups = $false
        try {
            $stagedVersionOutput = @(& $primaryStage version 2>&1)
            if ($LASTEXITCODE -ne 0) {
                throw "staged witself.exe failed its version self-test with exit code $LASTEXITCODE"
            }
            Assert-WitselfVersionOutput $stagedVersionOutput $versionWithoutPrefix 'staged witself.exe'

            Install-StagedFile `
                $primaryStage `
                $destination `
                $primaryBackup `
                ([ref]$hadPrimary) `
                ([ref]$primaryChanged) `
                ([ref]$primaryOriginalHash)
            if (Test-Path -LiteralPath $primaryBackup -PathType Leaf) {
                # A target appeared after preflight and was preserved by
                # File.Replace. Treat that mutation-time file as the original.
                $hadPrimary = $true
                $primaryBackupHash = $primaryOriginalHash
            }
            Install-StagedFile `
                $aliasStage `
                $alias `
                $aliasBackup `
                ([ref]$hadAlias) `
                ([ref]$aliasChanged) `
                ([ref]$aliasOriginalHash)
            if (Test-Path -LiteralPath $aliasBackup -PathType Leaf) {
                $hadAlias = $true
                $aliasBackupHash = $aliasOriginalHash
            }

            $versionOutput = @(& $destination version 2>&1)
            if ($LASTEXITCODE -ne 0) {
                throw "installed witself.exe failed its version self-test with exit code $LASTEXITCODE"
            }
            Assert-WitselfVersionOutput $versionOutput $versionWithoutPrefix 'installed witself.exe'
            $aliasOutput = @(& $alias version 2>&1)
            if ($LASTEXITCODE -ne 0) {
                throw "installed ws.exe alias failed its version self-test with exit code $LASTEXITCODE"
            }
            Assert-WitselfVersionOutput $aliasOutput $versionWithoutPrefix 'installed ws.exe alias'
            if ([string]::Join("`n", $aliasOutput) -ne [string]::Join("`n", $versionOutput)) {
                throw 'installed ws.exe alias reported a different version from witself.exe'
            }
            $primaryInstalledHash = (Get-FileHash -LiteralPath $destination -Algorithm SHA256).Hash.ToLowerInvariant()
            $aliasInstalledHash = (Get-FileHash -LiteralPath $alias -Algorithm SHA256).Hash.ToLowerInvariant()
            if ($primaryInstalledHash -ne $expectedInstalledHash -or $aliasInstalledHash -ne $expectedInstalledHash) {
                throw 'installed witself.exe and ws.exe do not match the verified release artifact'
            }
        } catch {
            $installFailure = $_.Exception.Message
            # File.Replace may have crossed its namespace boundary before
            # reporting an I/O failure. A backup is authoritative evidence that
            # replacement occurred and must participate in recovery, including
            # when the destination appeared after the initial preflight.
            if (Test-Path -LiteralPath $primaryBackup -PathType Leaf) {
                $hadPrimary = $true
                $primaryChanged = $true
                if ([string]::IsNullOrWhiteSpace($primaryBackupHash)) {
                    # A File.Replace error may still have created the backup.
                    # Fence it to the pre-call target hash; never bless bytes
                    # first observed only after the failure boundary.
                    $primaryBackupHash = $primaryOriginalHash
                }
            }
            if (Test-Path -LiteralPath $aliasBackup -PathType Leaf) {
                $hadAlias = $true
                $aliasChanged = $true
                if ([string]::IsNullOrWhiteSpace($aliasBackupHash)) {
                    $aliasBackupHash = $aliasOriginalHash
                }
            }
            $restorationErrors = New-Object System.Collections.Generic.List[string]
            foreach ($restore in @(
                @{
                    Target = $alias
                    Backup = $aliasBackup
                    HadOriginal = $hadAlias
                    Changed = $aliasChanged
                    ExpectedOriginalHash = $aliasOriginalHash
                    ExpectedBackupHash = $aliasBackupHash
                    TargetQuarantine = $aliasTargetQuarantine
                    BackupQuarantine = $aliasBackupQuarantine
                },
                @{
                    Target = $destination
                    Backup = $primaryBackup
                    HadOriginal = $hadPrimary
                    Changed = $primaryChanged
                    ExpectedOriginalHash = $primaryOriginalHash
                    ExpectedBackupHash = $primaryBackupHash
                    TargetQuarantine = $primaryTargetQuarantine
                    BackupQuarantine = $primaryBackupQuarantine
                }
            )) {
                try {
                    Restore-InstalledFile `
                        $restore.Target `
                        $restore.Backup `
                        ([bool]$restore.HadOriginal) `
                        ([bool]$restore.Changed) `
                        $expectedInstalledHash `
                        $restore.ExpectedOriginalHash `
                        $restore.ExpectedBackupHash `
                        $restore.TargetQuarantine `
                        $restore.BackupQuarantine
                } catch {
                    $restorationErrors.Add($_.Exception.Message)
                }
            }
            if ($restorationErrors.Count -gt 0) {
                $preserveBackups = $true
                $details = [string]::Join('; ', $restorationErrors)
                throw "installation failed ($installFailure) and restoration was incomplete ($details); recovery files were retained in $InstallDir"
            }
            throw "$installFailure; the previous installation was restored"
        } finally {
            foreach ($path in @($primaryStage, $aliasStage)) {
                if (Test-Path -LiteralPath $path) {
                    Remove-Item -LiteralPath $path -Force
                }
            }
            if (-not $preserveBackups) {
                foreach ($path in @($primaryBackup, $aliasBackup)) {
                    if (Test-Path -LiteralPath $path) {
                        Remove-Item -LiteralPath $path -Force
                    }
                }
            }
        }

        foreach ($line in $versionOutput) {
            Write-Host $line
        }
        Write-Host "Installed Witself to $destination"
        Write-Host "Installed alias to $alias"
        } finally {
            $installLock.Dispose()
        }
    } finally {
        if (Test-Path -LiteralPath $temporaryRoot) {
            Remove-Item -LiteralPath $temporaryRoot -Recurse -Force
        }
    }

    if (-not $NoPathUpdate) {
        Ensure-UserPath $InstallDir
    } else {
        Write-Host 'Skipped user PATH update (-NoPathUpdate).'
    }
    Write-Host 'Next: witself integrations'
    if ($NoPathUpdate) {
        Write-Host "Direct path: & '$destination' integrations"
    }
    if ($null -ne $componentVerifier) { Write-Output 'accepted' }
} catch {
    if (-not [string]::IsNullOrWhiteSpace($env:WS_COMPONENT_SET_DIR)) { throw (Get-WSComponentRefusal $_) }
    throw "install: $($_.Exception.Message)"
}
} $Version $InstallDir ([bool]$NoPathUpdate) $ReleaseRoot
