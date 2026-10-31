package webui

import (
	"net/http"
	"strconv"
)

// BackupInfoHandler serves public product and backup privacy information without
// consulting project data, credentials or the browser session. Mount the exact
// /about, /privacy and /backup-info.css paths alongside the browser handler.
func BackupInfoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w)
		if !safePath(r) {
			writeError(w, r, "not_found", "This information page is unavailable.", http.StatusNotFound)
			return
		}
		body, contentType := "", "text/html; charset=utf-8"
		switch r.URL.Path {
		case "/about":
			body = backupInfoHeader + backupAbout + backupInfoFooter
		case "/privacy":
			body = backupInfoHeader + backupPrivacy + backupInfoFooter
		case "/backup-info.css":
			body, contentType = backupInfoCSS, "text/css; charset=utf-8"
		default:
			writeError(w, r, "not_found", "This information page is unavailable.", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, r, "method_not_allowed", "Use GET or HEAD for information pages.", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(body))
		}
	})
}

const backupInfoHeader = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>FireController — Product and privacy</title><link rel="stylesheet" href="/backup-info.css"></head>
<body><header><a class="brand" href="/about">FireController</a><nav aria-label="Information"><a href="/about">About</a><a href="/privacy">Privacy</a><a href="https://github.com/benhook1013/FireController">Source code ↗</a></nav></header><main>`

const backupInfoFooter = `</main><footer>FireController is open source under Apache-2.0. <a href="https://github.com/benhook1013/FireController/issues">Project support ↗</a> · <a href="/privacy">Privacy</a></footer></body></html>`

const backupAbout = `<section class="intro"><p class="eyebrow">Coordination for AI development</p><h1>A clear view of your AI agents, their jobs, and your review and merge trains.</h1><p>FireController brings project work, agent conversations and code reviews into one shared service. Agents use bound CLI or MCP connections; people follow delivery and approved public project updates in the browser.</p></section>
<section><h2>Your projects, your installation</h2><p>Run FireController on your own computer or server. One installation can support several projects. Project credentials control private access, and each project chooses which information to publish.</p><p><a href="https://github.com/benhook1013/FireController">Explore FireController and its setup guides ↗</a></p></section>
<section><h2>Optional cloud backups</h2><p>FireController can send verified database and artifact backups to storage you choose, including Google Drive. An operator connects the storage account and configures a dedicated backup folder, retention and optional client-side encryption. Backup recovery uses the same format as local backups.</p><p>Google Drive access is used to create, verify, recover and retain FireController backups. The connection requests access to files created or explicitly opened with the app, rather than your entire Drive. Account authorization happens on Google's own sign-in page.</p><p>Backup data and account credentials stay with the installation you connect. The open-source project does not operate a central backup collection service.</p><p><a href="/privacy">Read how backup data and Google access are handled ↗</a></p></section>`

const backupPrivacy = `<h1>Privacy and cloud backups</h1><p class="muted">Effective 23 October 2026</p>
<section><h2>Who operates your installation</h2><p>FireController is an open-source project maintained through its public source repository. You or the organization running your installation controls its project data, access credentials, backup destinations and retention. Contact that operator about access to or deletion of private project data. Public project support is available through the <a href="https://github.com/benhook1013/FireController/issues">FireController repository</a>; do not post credentials, backups or private records there.</p></section>
<section><h2>What Google Drive access is used for</h2><p>When you connect Google Drive for backups, the backup process uses Google's Drive API through rclone to upload FireController backup archives and verification receipts, read them back to check integrity, download them for recovery and remove expired backups that it owns under the configured retention policy.</p><p>The connection requests <code>drive.file</code>: access to files created or explicitly opened with the app. It does not request permission to browse or modify all existing Drive files. FireController configures a dedicated backup folder and limits retention deletion to exact backup objects recorded as owned by that destination and instance.</p><p>Backup archives contain the installation's database and selected artifacts, which can include private jobs, messages, notes and review records. They should be treated as private even if some project information is published separately.</p></section>
<section><h2>Storage, protection and sharing</h2><p>Google account authorization occurs directly with Google. Access and refresh tokens are stored in the operator's private rclone configuration on the setup computer and/or backup server, as needed to authorize unattended backups. They are not included in FireController backup payloads or sent to the project's public information pages. Google receives API requests, backup files and their associated metadata in the account you select.</p><p>Backups remain in that account and in any local or recovery copies kept by the operator. Optional rclone crypt encryption protects file contents before upload; it is not enabled merely by choosing Google Drive. The operator must keep encryption recovery material separately.</p><p>The project does not collect backup contents centrally, sell Google user data, use it for advertising, or use it to train general-purpose AI models. Access is used only to provide the requested backup and recovery functions. Authorized operators and their chosen hosting and storage providers may have access according to the installation's configuration. FireController's use and transfer of information received from Google APIs adheres to the <a href="https://developers.google.com/terms/api-services-user-data-policy">Google API Services User Data Policy</a>, including its Limited Use requirements.</p></section>
<section><h2>Retention, disconnection and deletion</h2><p>The operator chooses how many verified offsite backups to retain. Automatic cleanup removes only recorded, owned backups and does not synchronize unrelated cloud files or mirror local deletions. Disconnecting does not erase existing backups.</p><p>To stop future access, disable the backup schedule and revoke FireController's access from your <a href="https://myaccount.google.com/connections">Google Account connections</a>. Remove private token files from setup computers and backup servers. To delete backup data, remove the dedicated folder in Drive and any local or recovery copies you no longer need. Revoking account access does not delete those copies or recover lost encryption keys.</p></section>
<section><h2>These public information pages</h2><p>The About and Privacy pages do not require a project login, set cookies or load analytics. Hosting infrastructure may record ordinary request information such as IP address, requested URL and time for operation and security. Other FireController pages use project sessions when you sign in; the operator controls private service data.</p><p>Changes to this policy will be published here. A change requiring additional Google permissions requires new consent before those permissions are used.</p></section>`

const backupInfoCSS = `:root{font-family:Inter,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;color:#211c1b;background:#f6f2ef;line-height:1.65}*{box-sizing:border-box}body{margin:0}header,main,footer{width:min(100% - 2.5rem,58rem);margin-inline:auto}header{display:flex;justify-content:space-between;align-items:center;gap:1rem;padding:1.5rem 0;border-bottom:1px solid #dbcac1}.brand{font-size:1.4rem;font-weight:700;letter-spacing:-.025em;color:#261715;text-decoration:none}nav{display:flex;flex-wrap:wrap;gap:1rem;font-size:.9rem}a{color:#80241d;text-underline-offset:.2em}a:hover{color:#481611}a:focus-visible{outline:2px solid #a72b20;outline-offset:4px}main{padding:2.5rem 0}h1,h2{line-height:1.2;letter-spacing:-.025em}h1{font-size:clamp(1.9rem,5vw,3rem);margin:0 0 1.2rem}h2{font-size:1.25rem;margin-top:0}p{margin:0 0 1rem}section{margin-bottom:2rem}section:not(.intro){border-top:1px solid #dbcac1;padding-top:1.5rem}.intro{padding:1.5rem;background:#efb3a2;border-left:5px solid #a72b20}.intro h1{font-size:clamp(1.8rem,4.5vw,2.8rem)}.eyebrow{font-size:.85rem;font-weight:700;text-transform:uppercase;letter-spacing:.08em;color:#78271e}.muted{color:#6c5b54}code{overflow-wrap:anywhere;font-size:.95em}footer{font-size:.85rem;color:#6c5b54;padding:1.5rem 0 2.5rem;border-top:1px solid #dbcac1}@media(max-width:580px){header{align-items:flex-start;flex-direction:column;padding-block:1rem}main{padding-block:1.5rem}.intro{padding:1rem}}`
