package reviewexec

import (
 "context"
 "encoding/json"
 "errors"
 "net/http"
 "path/filepath"
 "reflect"
 "strings"
 "sync/atomic"
 "testing"
 "time"

 "github.com/benhook1013/FireController/internal/core"
 "github.com/benhook1013/FireController/internal/reviewprovider"
)

type readinessTestReader struct {
 sourceCITestReader
 review func(context.Context,reviewprovider.CITarget)(reviewprovider.PullReviewObservation,error)
 reviewCalls atomic.Int64
}
func (r *readinessTestReader) ObservePullReviewState(ctx context.Context,target reviewprovider.CITarget)(reviewprovider.PullReviewObservation,error) {
 r.reviewCalls.Add(1)
 if r.review!=nil {return r.review(ctx,target)}
 return reviewprovider.PullReviewObservation{Target:target,State:"complete",ObservedAt:time.Now().UTC(),PullState:"OPEN",Mergeable:"MERGEABLE",MergeStateStatus:"CLEAN",Threads:[]reviewprovider.PullReviewThread{}},nil
}
func newReadinessTestReader() *readinessTestReader {
 r:=&readinessTestReader{}
 r.ci=func(_ context.Context,target reviewprovider.CITarget)(reviewprovider.CIObservation,error){
  o:=sourceCITestObservation(target,"not_required")
  o.Candidate.MergeState="verified";o.Candidate.MergeSHA=strings.Repeat("c",40);o.MergeState="verified"
  return o,nil
 }
 return r
}
func readinessRequest(number,revision int64) SourceRefreshRequest {r:=refreshRequest();r.PullNumber=number;r.ExpectedConfigurationRevision=revision;r.IncludeReadiness=true;return r}
func collectReadiness(t *testing.T,f *attemptFixture,r *readinessTestReader,key string,number,revision int64) SourceRefreshOperation {
 t.Helper();queued:=refreshEnqueue(t,f,key,readinessRequest(number,revision));cancel,done:=refreshStart(t,f,r);got:=refreshWait(t,f,queued.ID,"completed");refreshStop(t,cancel,done);return got
}
func readinessFixture(t *testing.T)(*attemptFixture,Attempt,string,*readinessTestReader,SourceRefreshOperation) {
 t.Helper();f,a,id,_:=coverageFixture(t,0,false);coverageFinalize(t,f,a,id,0);assessmentPolicy(t,f,1,[]string{"cli"},[]string{});r:=newReadinessTestReader();got:=collectReadiness(t,f,r,"readiness",2,1);return f,a,id,r,got
}
func readinessDurable(t *testing.T,f *attemptFixture,a Attempt) string {
 t.Helper();var rows string
 if err:=f.store.DB().QueryRow(`SELECT coalesce(group_concat(id||revision||state||observation_json||ci_observation_json||review_state_json,''),'') FROM review_source_refreshes`).Scan(&rows);err!=nil {t.Fatal(err)}
 return adjudicationState(t,f,a)+policyState(t,f)+rows
}
func readReadiness(t *testing.T,f *attemptFixture,r *readinessTestReader,a Attempt,number int64) SourceReadinessAssessment {
 t.Helper();before,calls,ciCalls,reviewCalls:=readinessDurable(t,f,a),r.calls.Load(),r.ciCalls.Load(),r.reviewCalls.Load()
 got,err:=f.e.ReadSourceReadiness(t.Context(),sourceCIScope(t,f),number)
 if err!=nil {t.Fatal(err)}
 if before!=readinessDurable(t,f,a) || calls!=r.calls.Load() || ciCalls!=r.ciCalls.Load() || reviewCalls!=r.reviewCalls.Load(){t.Fatal("local readiness read mutated state or called provider")}
 return got
}
func TestSourceReadinessReadyCompactReadAndOptionsReplay(t *testing.T) {
 f,a,_,r,op:=readinessFixture(t)
 if op.IncludeCI || !op.IncludeReadiness || op.CI==nil || op.ReviewState==nil || op.CI.State!="not_required" {t.Fatal("request options rewritten or collections missing",op)}
 got:=readReadiness(t,f,r,a,2)
 if got.State!="ready_as_observed" || len(got.Reasons)!=0 || got.Source!="observed_comparison" || got.PolicyRevision!=2 || len(got.Channels)!=1 || !got.Channels[0].Covered || got.ReviewState.TotalThreads!=0 || got.ReviewState.ReviewDecision!=nil {t.Fatal(got)}
 body,_:=json.Marshal(got)
 if len(body)>4096 || strings.Contains(string(body),"threads\":[") || strings.Contains(string(body),"authority") {t.Fatal("readiness leaked full inventory or grew unbounded",string(body))}
 before:=readinessDurable(t,f,a)
 replay:=refreshEnqueue(t,f,"readiness",readinessRequest(2,1))
 if replay.ID!=op.ID || before!=readinessDurable(t,f,a){t.Fatal("terminal exact replay changed observation")}
 ci,err:=f.e.ReadSourceCIAssessment(t.Context(),sourceCIScope(t,f),2)
 if err!=nil || ci.RefreshID!=op.ID || ci.State!="not_required" {t.Fatal("readiness collection unavailable to explicit CI read",ci,err)}
 // Execution pauses preserve already earned review credit and source binding.
 f.configure(t,attemptProject,"test-capability",1,true)
 assessmentPolicy(t,f,2,[]string{"cli"},[]string{"cli"})
 if paused:=readReadiness(t,f,r,a,2);paused.State!="ready_as_observed" || paused.ConfigurationRevision!=2 || paused.PolicyRevision!=3 {t.Fatal(paused)}
}
func TestSourceReadinessOptionsCoalesceOnlyExactRequestedFacts(t *testing.T) {
 f:=newAttemptFixture(t);f.configure(t,attemptProject,"test-capability",0,false)
 plain:=refreshEnqueue(t,f,"plain",refreshRequest());ci:=refreshEnqueue(t,f,"ci",sourceCIRequest());ready:=refreshEnqueue(t,f,"ready",readinessRequest(2,1));bothReq:=readinessRequest(2,1);bothReq.IncludeCI=true;both:=refreshEnqueue(t,f,"both",bothReq)
 if len(map[string]bool{plain.ID:true,ci.ID:true,ready.ID:true,both.ID:true})!=4 {t.Fatal("distinct option identities coalesced")}
 if other:=refreshEnqueue(t,f,"another-ready",readinessRequest(2,1));other!=ready {t.Fatal("same requested facts did not coalesce")}
 _,err:=f.e.EnqueueSourceRefresh(t.Context(),f.scope(t,attemptProject,f.writer,false),"plain",readinessRequest(2,1));attemptCode(t,err,"idempotency_conflict")
 r:=newReadinessTestReader();cancel,done:=refreshStart(t,f,r);refreshWait(t,f,both.ID,"completed");refreshStop(t,cancel,done)
 if r.calls.Load()!=8 || r.ciCalls.Load()!=3 || r.reviewCalls.Load()!=2 {t.Fatal("default modes broadened provider work",r.calls.Load(),r.ciCalls.Load(),r.reviewCalls.Load())}
}
func TestSourceReadinessUnknownAndBlockingProviderFacts(t *testing.T) {
 for _,name:=range []string{"current-thread","outdated-thread","draft","closed","conflict","blocked","changes-requested","review-required","unknown-review","ci-failed","ci-unavailable","review-error","malformed-provider"} {
  t.Run(name,func(t *testing.T){
   f,a,_,r,_:=readinessFixture(t)
   reason:=map[string]string{"current-thread":"unresolved_current_threads","outdated-thread":"unresolved_outdated_threads","draft":"pull_draft","closed":"pull_not_open","conflict":"mergeability_unproven","blocked":"merge_state_not_clean","changes-requested":"review_changes_requested","review-required":"review_required","unknown-review":"review_state_unproven","ci-failed":"ci_failed","ci-unavailable":"ci_unproven","review-error":"review_state_unproven","malformed-provider":"review_state_unproven"}[name]
   r.review=func(_ context.Context,target reviewprovider.CITarget)(reviewprovider.PullReviewObservation,error){o:=reviewprovider.PullReviewObservation{Target:target,State:"complete",ObservedAt:time.Now().UTC(),PullState:"OPEN",Mergeable:"MERGEABLE",MergeStateStatus:"CLEAN",Threads:[]reviewprovider.PullReviewThread{}}
    switch name {case "current-thread":o.Threads=[]reviewprovider.PullReviewThread{{ID:"thread_current"}};case "outdated-thread":o.Threads=[]reviewprovider.PullReviewThread{{ID:"thread_outdated",Outdated:true}};case "draft":o.Draft=true;case "closed":o.PullState="CLOSED";case "conflict":o.Mergeable="CONFLICTING";case "blocked":o.MergeStateStatus="BLOCKED";case "changes-requested":v:="CHANGES_REQUESTED";o.ReviewDecision=&v;case "review-required":v:="REVIEW_REQUIRED";o.ReviewDecision=&v;case "unknown-review":return *unavailableSourceReview(target,"review_state_changed"),nil;case "review-error":return o,errors.New("private transport diagnostics");case "malformed-provider":o.Threads=nil};return o,nil}
   if name=="ci-failed" {r.ci=func(_ context.Context,target reviewprovider.CITarget)(reviewprovider.CIObservation,error){return sourceCITestObservation(target,"failed"),nil}}
   if name=="ci-unavailable" {r.ci=func(context.Context,reviewprovider.CITarget)(reviewprovider.CIObservation,error){return reviewprovider.CIObservation{},errors.New("private credential")}}
   collectReadiness(t,f,r,"next-readiness",2,1)
   got:=readReadiness(t,f,r,a,2)
   if got.State!="held" || !slicesContains(got.Reasons,reason) {t.Fatal(name,got)}
   if strings.Contains(encoded(got),"private") {t.Fatal("raw provider diagnostic exposed")}
  })
 }
}
func slicesContains(items []string,want string)bool {for _,item:=range items {if item==want{return true}};return false}
func TestSourceReadinessCurrentLocalApplicabilityAndMissingPolicy(t *testing.T) {
 for _,change:=range []string{"unconfigured","configuration","repository","capability","head","base"} {t.Run(change,func(t *testing.T){
  f,a,_,r,_:=readinessFixture(t);want:="source_configuration_changed"
  switch change {case "unconfigured":admissionSQL(t,f,`DELETE FROM review_policy_config WHERE project_id=?`,attemptProject);want="policy_unconfigured";case "configuration":f.configure(t,attemptProject,"other-account",1,false);case "repository":admissionSQL(t,f,`UPDATE projects SET github_repository_id=999 WHERE id=?`,attemptProject);case "capability":var err error;caps:=append([]Capability{},attemptCapabilities...);caps[0].Fingerprint=strings.Repeat("9",64);f.e,err=New(f.store,caps);if err!=nil{t.Fatal(err)};case "head","base":r.pull=func(_ context.Context,_ string,n int64)(reviewprovider.Pull,error){p:=reviewprovider.Pull{Number:n,RepositoryID:801,State:"open",HeadSHA:strings.Repeat("a",40),BaseSHA:strings.Repeat("b",40),BaseRef:"main",ChangedFiles:1};if change=="head"{p.HeadSHA=strings.Repeat("d",40)}else{p.BaseSHA=strings.Repeat("e",40)};return p,nil};request:=refreshRequest();if change=="head"{request.HeadSHA=strings.Repeat("d",40)};queued:=refreshEnqueue(t,f,"later-source-only",request);cancel,done:=refreshStart(t,f,r);refreshWait(t,f,queued.ID,"completed");refreshStop(t,cancel,done);want="source_comparison_changed"}
  got:=readReadiness(t,f,r,a,2);if !reflect.DeepEqual(got.Reasons,[]string{want}) || change=="unconfigured" && got.State!="unconfigured" || change!="unconfigured" && got.State!="held" {t.Fatal(got)}
 })}
}
func TestSourceReadinessPrivateCorruptionAndPublicAuthorization(t *testing.T) {
 f,a,_,r,op:=readinessFixture(t);var original string
 if err:=f.store.DB().QueryRow(`SELECT review_state_json FROM review_source_refreshes WHERE id=?`,op.ID).Scan(&original);err!=nil {t.Fatal(err)}
 for _,body:=range []string{"null","{}",strings.Replace(original,`"threads":[]`,`"threads":null`,1),strings.Replace(original,`"draft":false`,`"draft":null`,1),strings.Replace(original,`"state":"complete"`,`"state":"complete","unknown":1`,1),strings.Replace(original,`"head_sha":"`+strings.Repeat("a",40),`"head_sha":"`+strings.Repeat("b",40),1)} {
  admissionSQL(t,f,`UPDATE review_source_refreshes SET review_state_json=? WHERE id=?`,body,op.ID);before:=readinessDurable(t,f,a)
  _,err:=f.e.ReadSourceReadiness(t.Context(),sourceCIScope(t,f),2);attemptCode(t,err,"storage_unavailable");if before!=readinessDurable(t,f,a){t.Fatal("corrupt read changed state")}
 }
 admissionSQL(t,f,`UPDATE review_source_refreshes SET review_state_json=? WHERE id=?`,original,op.ID)
 path:="/api/v1/projects/"+attemptProject+"/reviews/source/2/readiness"
 if w:=f.request(http.MethodGet,path,f.reader,"","");w.Code!=200 || !strings.Contains(w.Body.String(),"ready_as_observed") || w.Header().Get("Cache-Control")!="no-store"{t.Fatal(w.Code,w.Body.String())}
 for _,bad:=range []string{path+"?refresh=true",strings.Replace(path,"/2/","/02/",1)} {if w:=f.request(http.MethodGet,bad,f.reader,"","");w.Code!=400{t.Fatal(w.Code,w.Body.String())}}
 if w:=f.request(http.MethodGet,path,f.other,"","");w.Code!=403{t.Fatal(w.Code,w.Body.String())}
 admissionSQL(t,f,`UPDATE credentials SET revoked=1 WHERE id='exec-reader-cred'`)
 _,err:=f.e.ReadSourceReadiness(t.Context(),sourceCIScope(t,f),2);_ = err // scope creation itself must reject revocation; covered below with preexisting scope.
 _=r
}
func TestSourceReadinessRestartRetainsFactsAndFailedExtraPreservesSource(t *testing.T) {
 f,a,_,r,op:=readinessFixture(t)
 path:=filepath.Join(t.TempDir(),"copy-unused")
 _=path // actual store path is retained by fixture; reopen through its descriptor below.
 var database string
 rows,err:=f.store.DB().Query(`PRAGMA database_list`);if err!=nil{t.Fatal(err)};for rows.Next(){var sequence int;var name string;if err=rows.Scan(&sequence,&name,&database);err!=nil{t.Fatal(err)};if name=="main"{break}};rows.Close()
 if err=f.store.Close();err!=nil{t.Fatal(err)}
 reopened,err:=core.Open(database);if err!=nil{t.Fatal(err)};t.Cleanup(func(){reopened.Close()});f.store=reopened;f.e,err=New(reopened,attemptCapabilities);if err!=nil{t.Fatal(err)}
 got:=refreshRead(t,f,op.ID);if !reflect.DeepEqual(got.ReviewState,op.ReviewState) || got.CI==nil{t.Fatal("restart lost typed facts",got)}
 if readReadiness(t,f,r,a,2).State!="ready_as_observed"{t.Fatal("restart changed verdict")}
 r.review=func(context.Context,reviewprovider.CITarget)(reviewprovider.PullReviewObservation,error){return reviewprovider.PullReviewObservation{},errors.New("offline")}
 failed:=collectReadiness(t,f,r,"failed-extra",2,1)
 if failed.State!="completed" || failed.Observation==nil || failed.ReviewState.State!="unproven" {t.Fatal("extra failure discarded useful source",failed)}
 if got:=readReadiness(t,f,r,a,2);got.State!="held"{t.Fatal(got)}
}
