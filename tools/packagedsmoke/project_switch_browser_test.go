//go:build packaged_smoke && linux

package packagedsmoke

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/server"
)

// TestPackagedProjectSwitchBrowserSmoke consumes an already-built fcd and local
// Playwright installation. Trusted stopped setup is synthetic; browser traffic
// uses the actual embedded UI and real cookie-authenticated project switching.
// Optional FC_PROJECT_SWITCH_SCREENSHOTS retains masked desktop/phone PNGs in an
// absolute, owner-private directory outside the checkout. No traces are saved.
func TestPackagedProjectSwitchBrowserSmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	t.Cleanup(cancel) // Daemon cleanup must run before cancellation on failure.
	private := privateFixtureDirectory(t)
	if os.Chmod(private, 0700) != nil {
		t.Fatal("private project-switch fixture unavailable")
	}
	binDir := os.Getenv("FC_SMOKE_BIN_DIR")
	if binDir == "" {
		binDir, _ = filepath.Abs("../../bin")
	}
	if !filepath.IsAbs(binDir) {
		t.Fatal("FC_SMOKE_BIN_DIR must be absolute")
	}
	binary := filepath.Join(binDir, "fcd")
	hash := binaryHash(t, binary)
	t.Logf("project-switch packaged input fcd sha256=%x", hash)
	defer func() {
		if binaryHash(t, binary) != hash {
			t.Error("project-switch proof modified fcd")
		}
	}()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node required for project-switch browser proof")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal("Node executable unavailable")
	}
	var external, engine atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(502) }))
	defer deny.Close()
	browserDeny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { engine.Add(1); w.WriteHeader(502) }))
	defer browserDeny.Close()
	env := []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "TEMP=" + private, "TMP=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, binary, data, env)
	health(t, ctx, bootstrap.address)
	adminFile := filepath.Join(data, "bootstrap-admin.token")
	admin, err := os.ReadFile(adminFile)
	if err != nil || len(bytes.TrimSpace(admin)) == 0 {
		t.Fatal("synthetic bootstrap credential unavailable")
	}
	bootstrap.stop(t)
	projects, credentialFile := seedProjectSwitchBrowser(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	daemon := startDaemon(t, ctx, binary, data, env)
	if daemon.instance != bootstrap.instance {
		t.Fatal("project-switch fixture identity changed")
	}
	health(t, ctx, daemon.address)
	executable := os.Getenv("FC_BROWSER_EXECUTABLE")
	if executable != "" {
		info, err := os.Stat(executable)
		if !filepath.IsAbs(executable) || err != nil || !info.Mode().IsRegular() {
			t.Fatal("FC_BROWSER_EXECUTABLE must select an absolute regular executable")
		}
		t.Log("project-switch proof uses explicit browser executable override")
	}
	browserPath := os.Getenv("PLAYWRIGHT_BROWSERS_PATH")
	if browserPath == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			t.Fatal("Playwright cache unavailable")
		}
		browserPath = filepath.Join(cache, "ms-playwright")
	}
	if !filepath.IsAbs(browserPath) {
		t.Fatal("PLAYWRIGHT_BROWSERS_PATH must be absolute")
	}
	env = append(env, "PLAYWRIGHT_BROWSERS_PATH="+browserPath)
	playwright, _ := filepath.Abs("../../webapp/node_modules/@playwright/test/index.mjs")
	config := map[string]any{"origin": "http://" + daemon.address, "instance": daemon.instance, "projects": projects, "credential_file": credentialFile, "admin_file": adminFile, "playwright": playwright, "executable": executable, "proxy": browserDeny.URL}
	if directory := os.Getenv("FC_PROJECT_SWITCH_SCREENSHOTS"); directory != "" {
		root, _ := filepath.Abs("../..")
		rel, err := filepath.Rel(root, directory)
		if !filepath.IsAbs(directory) || err != nil || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatal("FC_PROJECT_SWITCH_SCREENSHOTS must be absolute and outside the checkout")
		}
		if os.MkdirAll(directory, 0700) != nil {
			t.Fatal("private screenshot directory unavailable")
		}
		resolved, err := filepath.EvalSymlinks(directory)
		info, statErr := os.Lstat(directory)
		if err != nil || statErr != nil || resolved != filepath.Clean(directory) || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			t.Fatal("screenshot directory must be owner-private and have no symlink ancestors")
		}
		config["screenshots"] = directory
	}
	raw, err := json.Marshal(config)
	configFile := filepath.Join(private, "project-switch-browser.json")
	if err != nil || os.WriteFile(configFile, raw, 0600) != nil {
		t.Fatal("private project-switch configuration unavailable")
	}
	command := exec.Command(node, "--input-type=module", "-", configFile)
	command.Stdin = strings.NewReader(projectSwitchBrowserDriver)
	command.Env, command.SysProcAttr, command.WaitDelay = env, &syscall.SysProcAttr{Setpgid: true}, time.Second
	output, stderr := &browserOutput{}, &boundedOutput{}
	command.Stdout, command.Stderr = output, stderr
	if command.Start() != nil {
		t.Fatal("project-switch browser driver could not start")
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	joined := false
	defer func() {
		if pid := output.browserPID(); pid > 1 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if !joined {
			select {
			case <-done:
			case <-time.After(stepTimeout):
				t.Error("owned project-switch driver did not join")
			}
		}
	}()
	select {
	case err = <-done:
		joined = true
	case <-ctx.Done():
		t.Fatal("project-switch workload exceeded 65 seconds; raw output withheld")
	}
	lines := bytes.Split(bytes.TrimSpace(output.bytes()), []byte("\n"))
	var result struct {
		State       string `json:"state"`
		Phase       string `json:"phase"`
		Viewports   int    `json:"viewports"`
		Logins      int    `json:"logins"`
		Switches    int    `json:"switches"`
		Logouts     int    `json:"logouts"`
		Screenshots int    `json:"screenshots"`
		Requests    int    `json:"requests"`
		Errors      int    `json:"errors"`
		External    int    `json:"external"`
		Writes      int    `json:"writes"`
		Leaks       int    `json:"leaks"`
	}
	if len(lines) > 0 {
		_ = json.Unmarshal(lines[len(lines)-1], &result)
	}
	// Never print raw browser output, assertion details, URLs, cookies or tokens.
	if err != nil || stderr.len() != 0 || output.len() > 64<<10 || len(lines) != 2 || result.State != "PASS" || result.Viewports != 2 || result.Logins != 2 || result.Switches != 8 || result.Logouts != 2 || result.Requests == 0 || result.Errors != 0 || result.External != 0 || result.Writes != 0 || result.Leaks != 0 || output.browserPID() < 2 {
		phase := "driver"
		switch result.Phase {
		case "configuration", "launch", "login", "source", "switch", "repeat", "reload", "worker", "screenshot", "logout", "cleanup":
			phase = result.Phase
		}
		t.Fatalf("project-switch browser failed: phase=%s process_error=%t stderr_bytes=%d; private output withheld", phase, err != nil, stderr.len())
	}
	wantScreenshots := 0
	if config["screenshots"] != nil {
		wantScreenshots = 4
	}
	if result.Screenshots != wantScreenshots {
		t.Fatal("project-switch screenshot proof incomplete")
	}
	daemon.stop(t)
	if external.Load() != 0 {
		t.Fatal("project-switch daemon attempted external HTTP")
	}
	for _, pid := range []int{-output.browserPID(), -command.Process.Pid, daemon.command.Process.Pid} {
		if pid == 0 || pid == -1 || syscall.Kill(pid, 0) != syscall.ESRCH {
			t.Error("owned project-switch process survived shutdown")
		}
	}
	t.Logf("project-switch passed: viewports=1440,390 logins=2 switches=8 logouts=2 screenshots=%d application_requests=%d browser_domain_writes=0 external_http=0 blocked_engine_requests=%d", result.Screenshots, result.Requests, engine.Load())
}

type projectSwitchBrowserProject struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Handle     string   `json:"handle"`
	Worker     string   `json:"worker"`
	WorkerName string   `json:"worker_name"`
	Workers    []string `json:"workers"`
	Job        string   `json:"job"`
	Title      string   `json:"title"`
	Brief      string   `json:"brief"`
}

func seedProjectSwitchBrowser(t *testing.T, ctx context.Context, data, instance, admin, private string) ([]projectSwitchBrowserProject, string) {
	t.Helper()
	store, err := core.Open(ctx, data)
	if err != nil {
		t.Fatal("stopped project-switch fixture unavailable")
	}
	defer store.Close()
	if store.InstanceID() != instance {
		t.Fatal("stopped project-switch identity changed")
	}
	projects := []projectSwitchBrowserProject{
		{Name: "Aster workshop", Handle: "switch-aster", WorkerName: "Aster builder", Title: "Aster private delivery", Brief: "Aster private brief: prepare the synthetic release."},
		{Name: "Birch workshop", Handle: "switch-birch", WorkerName: "Birch reader", Title: "Birch private delivery", Brief: "Birch private brief: inspect the synthetic handoff."},
		{Name: "Cedar restricted", Handle: "switch-cedar", WorkerName: "Cedar hidden worker", Title: "Cedar private delivery", Brief: "Cedar private brief must never be offered."},
	}
	lookups := 0
	mux := server.New(store, server.WithRepositoryResolver(func(_ context.Context, repository string) (server.RepositoryIdentity, error) {
		for i, project := range projects {
			if repository == "synthetic/"+project.Handle {
				lookups++
				return server.RepositoryIdentity{ID: int64(7101 + i), FullName: repository}, nil
			}
		}
		return server.RepositoryIdentity{}, core.Fail("provider_unavailable", "Unowned synthetic repository.", 503)
	}))
	jobs.Register(mux, store)
	call := func(path string, body any, status int) string {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal("project-switch fixture JSON unavailable")
		}
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+admin)
		r.Header.Set("X-FC-Instance", instance)
		r.Header.Set("Idempotency-Key", core.NewID("switch_fixture"))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		var receipt struct{ ID string }
		if w.Code != status || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.ID == "" {
			t.Fatalf("trusted project-switch setup refused: status=%d expected=%d; private response withheld", w.Code, status)
		}
		return receipt.ID
	}
	for i := range projects {
		p := &projects[i]
		p.ID = call("/api/v1/projects", map[string]any{"name": p.Name, "handle": p.Handle, "repository": "synthetic/" + p.Handle}, 200)
		prefix := "/api/v1/projects/" + p.ID
		p.Worker = call(prefix+"/workers", map[string]any{"name": p.WorkerName}, 200)
		other := call(prefix+"/workers", map[string]any{"name": p.WorkerName + " colleague"}, 200)
		p.Workers = []string{p.Worker, other}
		p.Job = call(prefix+"/jobs", map[string]any{"title": p.Title, "brief": p.Brief, "worker_id": p.Worker}, 201)
		call(prefix+"/jobs", map[string]any{"title": p.Name + " follow-up", "brief": "Synthetic project-local follow-up.", "worker_id": other}, 201)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal("synthetic personal token unavailable")
	}
	token := hex.EncodeToString(secret)
	digest := sha256.Sum256([]byte(token))
	// Ordinary instance-admin setup, not a connector or instance-admin login.
	call("/api/v1/operator/browser-credentials", map[string]any{"token_sha256": hex.EncodeToString(digest[:]), "projects": []any{
		map[string]any{"project_id": projects[0].ID, "permissions": []string{"manage"}},
		map[string]any{"project_id": projects[1].ID, "permissions": []string{"read"}},
	}}, 200)
	for i, p := range projects {
		for _, permission := range []string{"read", "write", "manage"} {
			r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
			r.SetPathValue("project_id", p.ID)
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("X-FC-Instance", instance)
			var accessErr error
			if permission == "manage" {
				_, accessErr = store.AdminScope(r)
			} else {
				_, accessErr = store.Scope(r, permission)
			}
			allowed := i == 0 || i == 1 && permission == "read"
			if (accessErr == nil) != allowed {
				t.Fatal("synthetic personal credential permission boundary invalid")
			}
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+token)
	if _, err := store.AdminScope(r); err == nil || lookups != 3 {
		t.Fatal("synthetic personal credential acquired instance authority or unexpected repository lookup")
	}
	credential := filepath.Join(private, "personal.token")
	if os.WriteFile(credential, []byte(token+"\n"), 0600) != nil {
		t.Fatal("private personal credential file unavailable")
	}
	return projects, credential
}

// This focused check needs Node only: it parses the embedded driver without
// launching Chromium, contacting a daemon, reading a profile or installing npm.
func TestProjectSwitchBrowserDriverSyntax(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
	defer cancel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node required for project-switch driver syntax check")
	}
	command := exec.CommandContext(ctx, node, "--input-type=module", "--check")
	command.Stdin = strings.NewReader(projectSwitchBrowserDriver)
	command.Env = []string{"PATH="}
	output := &boundedOutput{}
	command.Stdout, command.Stderr = output, output
	if command.Run() != nil || output.len() != 0 {
		t.Fatal("embedded project-switch browser driver syntax invalid; output withheld")
	}
}

const projectSwitchBrowserDriver = `
import fs from 'node:fs/promises';
import path from 'node:path';
import {pathToFileURL} from 'node:url';
let server,browser,context,phase='configuration',passed=false;
const facts={viewports:0,logins:0,switches:0,logouts:0,session_deletes:0,screenshots:0,requests:0,errors:0,external:0,writes:0,leaks:0};
const need=value=>{if(!value)throw new Error('assertion');};
try {
  const config=JSON.parse(await fs.readFile(process.argv[2],'utf8'));
  const {chromium,expect}=await import(pathToFileURL(config.playwright));
  const [token,admin]=await Promise.all([config.credential_file,config.admin_file].map(async file=>(await fs.readFile(file,'utf8')).trim()));
  const [a,b,c]=config.projects,key='firecontroller.browser-session.v1';
  for(const p of config.projects){p.home='/projects/'+p.handle;p.prefix='/api/v1/projects/'+p.id;}
  phase='launch';
  server=await chromium.launchServer({headless:true,timeout:5000,proxy:{server:config.proxy,bypass:'127.0.0.1,localhost'},...(config.executable?{executablePath:config.executable}:{}),args:['--disable-background-networking','--no-first-run']});
  console.log(JSON.stringify({event:'browser_started',pid:server.process().pid}));
  browser=await chromium.connect(server.wsEndpoint(),{timeout:5000});
  for(const width of [1440,390]) {
    context=await browser.newContext({viewport:{width,height:width===390?844:1000},serviceWorkers:'block'});
    let logins=0,switches=0;
    const page=await context.newPage();page.setDefaultTimeout(5000);page.setDefaultNavigationTimeout(5000);
    const unpublishedLocation=value=>{try{const url=new URL(value);return url.origin===config.origin&&[a,b].some(p=>url.pathname==='/api/v1/public/project-locations/'+p.handle)&&url.search==='';}catch{return false;}};
    page.on('pageerror',()=>facts.errors++);
    page.on('console',message=>{
      if(message.type()!=='error'||unpublishedLocation(message.location().url)&&/^Failed to load resource: the server responded with a status of 404 \(Not Found\)$/u.test(message.text()))return;
      facts.errors++;
    });
    page.on('response',response=>{if(response.status()>=400&&!(response.status()===404&&response.request().method()==='GET'&&unpublishedLocation(response.url())))facts.errors++;});
    await context.route('**/*',async route=>{
      const request=route.request(),url=new URL(request.url()),method=request.method(),headers=request.headers();
      if(url.origin!==config.origin){facts.external++;await route.abort();return;}
      if(url.pathname.startsWith('/api/'))facts.requests++;
      const project=/^\/api\/v1\/projects\/([^/]+)/u.exec(url.pathname);
      if(project){
        const p=[a,b].find(p=>[p.id,p.handle].includes(decodeURIComponent(project[1])));
        if(!p||headers['x-fc-worker']&&!p.workers.includes(headers['x-fc-worker'])){facts.leaks++;await route.abort();return;}
      }
      if(!['GET','HEAD'].includes(method)){
        if(!/\/browser-session(?:\/[^/]+)?$/u.test(url.pathname)){facts.writes++;await route.abort();return;}
        if(method==='POST'&&headers.authorization){
          need(headers.authorization==='Bearer '+token&&url.pathname===a.prefix+'/browser-session');
          const body=request.postDataJSON();need(body.project_switching===true&&Object.keys(body).sort().join(',')==='project_switching,session_id');
          logins++;facts.logins++;
        }
        else if(method==='DELETE'){facts.session_deletes++;}
        else if(method==='POST'&&!url.pathname.endsWith('/renew')){switches++;facts.switches++;}
      }
      await route.continue();
    });
    // Observe actual rendered DOM transitions, including reload. The observer
    // retains booleans/counters only, not DOM text, tokens or request captures.
    await context.addInitScript(({projects,key})=>{
      window.__switchQA={leaks:0,forms:0,watch:!!sessionStorage.getItem(key)};
      const inspect=()=>{
        const qa=window.__switchQA,body=document.body;if(!body)return;
        const current=projects.find(p=>location.pathname==='/projects/'+p.handle||location.pathname.startsWith('/projects/'+p.handle+'/'));
        if(current){
          const text=body.innerText;
          if(projects.filter(p=>p.id!==current.id).some(p=>[p.title,p.brief,p.worker_name,...p.workers,p.job].some(s=>text.includes(s))))qa.leaks++;
          try {const binding=JSON.parse(sessionStorage.getItem(key));if(binding?.projectID===current.id&&binding.selectedworkerID&&!current.workers.includes(binding.selectedworkerID))qa.leaks++;}catch{}
        }
        if(qa.watch&&document.querySelector('input[type=password]')?.getBoundingClientRect().height)qa.forms++;
      };
      new MutationObserver(inspect).observe(document,{subtree:true,childList:true,characterData:true,attributes:true});
    },{projects:config.projects,key});
    const binding=()=>page.evaluate(key=>JSON.parse(sessionStorage.getItem(key)),key);
    const nav=name=>page.getByRole('navigation',{name:'Project navigation'}).getByRole('link',{name,exact:true});
    const chooser=()=>page.getByLabel('Switch project',{exact:true});
    const safe=async()=>{
      const state=await binding(),cookies=await context.cookies();
      const secrets=[token,admin,state?.sessionID,...cookies.flatMap(cookie=>[cookie.name,cookie.value])].filter(Boolean);
      const clean=await page.evaluate(secrets=>{
        const html=document.documentElement.outerHTML,values=[...document.querySelectorAll('input,textarea')].map(e=>e.value).join('\n');
        const storage=JSON.stringify([Object.entries(sessionStorage),Object.entries(localStorage)]);
        return !secrets.some(s=>html.includes(s)||values.includes(s))&&!storage.includes(secrets[0])&&!storage.includes(secrets[1])&&!location.href.includes(secrets[0]);
      },secrets);need(clean);
    };
    const settled=async(p,other)=>{
      await expect(chooser()).toBeVisible();await expect(chooser()).toBeEnabled();
      await expect.poll(async()=>chooser().locator('option').evaluateAll(options=>options.filter(o=>o.value&&!o.disabled).map(o=>o.value).sort())).toEqual([a.id,b.id].sort());
      await expect(chooser()).toHaveValue(p.id);
      const saved=await binding();need(saved.projectID===p.id&&saved.instanceID===config.instance&&(!saved.selectedworkerID||p.workers.includes(saved.selectedworkerID)));
      need(new URL(page.url()).pathname===p.home||new URL(page.url()).pathname.startsWith(p.home+'/'));
      const text=await page.locator('body').innerText();need(![other.title,other.brief,other.worker_name,...other.workers,other.job,c.name,c.title,c.brief,c.id,c.worker].some(value=>text.includes(value)));
      const authority=await page.evaluate(async({prefix,key})=>{const s=JSON.parse(sessionStorage.getItem(key));const r=await fetch(prefix+'/browser-session',{credentials:'same-origin',headers:{'X-FC-Instance':s.instanceID,'X-FC-Browser-Session':s.sessionID}});return {status:r.status,value:await r.json()};},{prefix:p.prefix,key});
      need(authority.status===200&&authority.value.project_id===p.id&&authority.value.instance_id===config.instance&&authority.value.session_id===saved.sessionID&&authority.value.allow_writes===(p.id===a.id)&&authority.value.can_edit_presentation===(p.id===a.id));
      if(p.id===a.id)await expect(nav('Settings')).toBeVisible();else await expect(nav('Settings')).toHaveCount(0);
      await expect(page.getByRole('status',{name:'Project updates: connected',exact:true})).toBeVisible();
      const qa=await page.evaluate(()=>window.__switchQA);need(!qa.leaks&&!qa.forms);
      need(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
      await safe();return saved;
    };
    const ownWorker=async p=>{
      phase='worker';await nav('Workers').click();await page.getByRole('button',{name:'Open '+p.worker_name+' inbox',exact:true}).click();
      await expect(page.getByRole('heading',{name:p.worker_name+' inbox',exact:true})).toBeVisible();
      need((await binding()).selectedworkerID===p.worker);
      if(p.id===a.id)await expect(page.getByRole('button',{name:'New message',exact:true})).toBeVisible();else await expect(page.getByRole('button',{name:'New message',exact:true})).toHaveCount(0);
    };
    const photograph=async p=>{
      phase='screenshot';await nav('Delivery').click();await expect(page.getByRole('link',{name:p.title,exact:true})).toBeVisible();await settled(p,p.id===a.id?b:a);
      await page.evaluate(()=>scrollTo(0,0));
      const box=await chooser().boundingBox();need(box&&box.width>0&&box.x>=0&&box.x+box.width<=width&&box.y>=0);
      if(config.screenshots){
        // Same QA masking convention as the existing packaged browser driver.
        // Buffer + exclusive private write never overwrites another proof run.
        const png=await page.screenshot({fullPage:true,timeout:2000,mask:[page.locator('input,textarea,code,.full-text,.message-preview')]});
        await fs.writeFile(path.join(config.screenshots,'project-switch-'+(width===390?'phone':'desktop')+'-'+(p.id===a.id?'aster':'birch')+'.png'),png,{mode:0o600,flag:'wx'});facts.screenshots++;
      }
    };
    const repeatAndReload=async(p,other)=>{
      phase='repeat';const before=await binding(),posts=switches;
      await chooser().selectOption(p.id);await settled(p,other);
      need((await binding()).sessionID===before.sessionID&&switches===posts);
      phase='reload';await page.reload();await settled(p,other);
      const after=await binding();need(after.sessionID===before.sessionID&&after.selectedworkerID===before.selectedworkerID&&logins===1);
    };
    const change=async(p,other)=>{
      phase='switch';const before=switches;await chooser().selectOption(p.id);const saved=await settled(p,other);need(switches===before+1&&logins===1&&saved.selectedworkerID==='');
    };
    phase='login';await page.goto(config.origin+a.home+'/jobs/'+a.job);
    await page.getByLabel('Project credential',{exact:true}).fill(token);
    await expect(page.getByRole('checkbox')).toHaveCount(0);
    await page.getByRole('button',{name:'Connect to project',exact:true}).click();
    await expect(page.getByText(a.brief,{exact:true})).toBeVisible();
    await page.evaluate(()=>{window.__switchQA.watch=true;});
    phase='source';await settled(a,b);need(logins===1);await ownWorker(a);await photograph(a);
    await change(b,a);await repeatAndReload(b,a);await ownWorker(b);await photograph(b);
    await change(a,b);await ownWorker(a);await repeatAndReload(a,b);
    // Repeat both directions after each project has had an active worker.
    await change(b,a);await settled(b,a);await change(a,b);await settled(a,b);
    need(logins===1&&switches===4);
    phase='logout';await page.evaluate(()=>{window.__switchQA.watch=false;});
    const ended=page.waitForResponse(response=>response.request().method()==='DELETE'&&new URL(response.url()).pathname===a.prefix+'/browser-session');
    await page.getByRole('button',{name:'Sign out',exact:true}).click();need((await ended).status()===200);facts.logouts++;
    await expect(page.getByRole('button',{name:'Connect to project',exact:true})).toBeVisible();
    need(await binding()===null);
    const html=await page.content();need(![a.title,a.brief,a.worker_name,b.title,b.brief,b.worker_name,token,admin].some(s=>html.includes(s)));
    facts.viewports++;await context.close();context=undefined;
  }
  need(facts.logins===2&&facts.switches===8&&facts.logouts===2&&!facts.errors&&!facts.external&&!facts.writes&&!facts.leaks);passed=true;
}catch{process.exitCode=1;}
finally{
  const failedPhase=phase;phase='cleanup';
  try{await context?.close();await browser?.close();await server?.close();}catch{passed=false;process.exitCode=1;}
  console.log(JSON.stringify({state:passed?'PASS':'FAIL',phase:passed?'cleanup':failedPhase,...facts}));
}
`
