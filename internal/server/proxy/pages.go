package proxy

import (
	"net/http"
	"time"

	json "github.com/goccy/go-json"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"drip/internal/shared/httputil"
)

func (h *Handler) serveHomePage(w http.ResponseWriter, r *http.Request) {
	html := `<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8" />
	<meta name="viewport" content="width=device-width, initial-scale=1.0" />
	<title>Drip - Your Tunnel, Your Domain, Anywhere</title>
	` + faviconLink + `
	<style>
		* { margin: 0; padding: 0; box-sizing: border-box; }
		body {
			font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
			background: #fff;
			color: #24292f;
			line-height: 1.6;
		}
		.container { max-width: 720px; margin: 0 auto; padding: 48px 24px; }
		header { margin-bottom: 48px; }
		h1 { font-size: 28px; font-weight: 600; margin-bottom: 8px; }
		h1 span { margin-right: 8px; }
		.desc { color: #57606a; font-size: 16px; }
		h2 { font-size: 18px; font-weight: 600; margin: 32px 0 12px; }
		.code-wrap {
			position: relative;
			background: #f6f8fa;
			border: 1px solid #d0d7de;
			border-radius: 6px;
			margin-bottom: 12px;
		}
		.code-wrap pre {
			margin: 0;
			padding: 12px 16px;
			padding-right: 60px;
			font-family: ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, monospace;
			font-size: 14px;
			overflow-x: auto;
			white-space: pre-wrap;
			word-break: break-all;
		}
		.copy-btn {
			position: absolute;
			top: 8px;
			right: 8px;
			background: #fff;
			border: 1px solid #d0d7de;
			border-radius: 6px;
			padding: 4px 6px;
			cursor: pointer;
			color: #57606a;
			display: flex;
			align-items: center;
			justify-content: center;
		}
		.copy-btn:hover { background: #f3f4f6; }
		.copy-btn svg { width: 16px; height: 16px; }
		.copy-btn .check { display: none; color: #1a7f37; }
		.copy-btn.copied .copy { display: none; }
		.copy-btn.copied .check { display: block; }
		.links { margin-top: 32px; display: flex; gap: 24px; flex-wrap: wrap; }
		.links a { color: #0969da; text-decoration: none; font-size: 14px; }
		.links a:hover { text-decoration: underline; }
		footer { margin-top: 48px; padding-top: 24px; border-top: 1px solid #d0d7de; }
		footer a { color: #57606a; text-decoration: none; font-size: 14px; }
		footer a:hover { color: #0969da; }
	</style>
</head>
<body>
	<div class="container">
		<header>
			<h1><span>💧</span>Drip</h1>
			<p class="desc">Your Tunnel, Your Domain, Anywhere</p>
		</header>

		<p>A self-hosted tunneling solution to securely expose your services to the internet.</p>

		<h2>Install</h2>
		<div class="code-wrap">
			<pre>VERSION=v0.7.0
SCRIPT=install.sh
curl -fsSLO "https://raw.githubusercontent.com/Gouryella/drip/${VERSION}/scripts/${SCRIPT}"
# Verify the script and release archive SHA256 before running.
bash "$SCRIPT" --version "$VERSION" --installer-checksum "&lt;installer_sha256&gt;" --checksum "&lt;archive_sha256&gt;"</pre>
			<button class="copy-btn" onclick="copy(this)">
				<svg class="copy" viewBox="0 0 16 16" fill="currentColor"><path d="M0 6.75C0 5.784.784 5 1.75 5h1.5a.75.75 0 0 1 0 1.5h-1.5a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-1.5a.75.75 0 0 1 1.5 0v1.5A1.75 1.75 0 0 1 9.25 16h-7.5A1.75 1.75 0 0 1 0 14.25Z"></path><path d="M5 1.75C5 .784 5.784 0 6.75 0h7.5C15.216 0 16 .784 16 1.75v7.5A1.75 1.75 0 0 1 14.25 11h-7.5A1.75 1.75 0 0 1 5 9.25Zm1.75-.25a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-7.5a.25.25 0 0 0-.25-.25Z"></path></svg>
				<svg class="check" viewBox="0 0 16 16" fill="currentColor"><path d="M13.78 4.22a.75.75 0 0 1 0 1.06l-7.25 7.25a.75.75 0 0 1-1.06 0L2.22 9.28a.751.751 0 0 1 .018-1.042.751.751 0 0 1 1.042-.018L6 10.94l6.72-6.72a.75.75 0 0 1 1.06 0Z"></path></svg>
			</button>
		</div>

		<h2>Usage</h2>
		<div class="code-wrap">
			<pre>drip http 3000</pre>
			<button class="copy-btn" onclick="copy(this)">
				<svg class="copy" viewBox="0 0 16 16" fill="currentColor"><path d="M0 6.75C0 5.784.784 5 1.75 5h1.5a.75.75 0 0 1 0 1.5h-1.5a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-1.5a.75.75 0 0 1 1.5 0v1.5A1.75 1.75 0 0 1 9.25 16h-7.5A1.75 1.75 0 0 1 0 14.25Z"></path><path d="M5 1.75C5 .784 5.784 0 6.75 0h7.5C15.216 0 16 .784 16 1.75v7.5A1.75 1.75 0 0 1 14.25 11h-7.5A1.75 1.75 0 0 1 5 9.25Zm1.75-.25a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-7.5a.25.25 0 0 0-.25-.25Z"></path></svg>
				<svg class="check" viewBox="0 0 16 16" fill="currentColor"><path d="M13.78 4.22a.75.75 0 0 1 0 1.06l-7.25 7.25a.75.75 0 0 1-1.06 0L2.22 9.28a.751.751 0 0 1 .018-1.042.751.751 0 0 1 1.042-.018L6 10.94l6.72-6.72a.75.75 0 0 1 1.06 0Z"></path></svg>
			</button>
		</div>
		<div class="code-wrap">
			<pre>drip https 443</pre>
			<button class="copy-btn" onclick="copy(this)">
				<svg class="copy" viewBox="0 0 16 16" fill="currentColor"><path d="M0 6.75C0 5.784.784 5 1.75 5h1.5a.75.75 0 0 1 0 1.5h-1.5a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-1.5a.75.75 0 0 1 1.5 0v1.5A1.75 1.75 0 0 1 9.25 16h-7.5A1.75 1.75 0 0 1 0 14.25Z"></path><path d="M5 1.75C5 .784 5.784 0 6.75 0h7.5C15.216 0 16 .784 16 1.75v7.5A1.75 1.75 0 0 1 14.25 11h-7.5A1.75 1.75 0 0 1 5 9.25Zm1.75-.25a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-7.5a.25.25 0 0 0-.25-.25Z"></path></svg>
				<svg class="check" viewBox="0 0 16 16" fill="currentColor"><path d="M13.78 4.22a.75.75 0 0 1 0 1.06l-7.25 7.25a.75.75 0 0 1-1.06 0L2.22 9.28a.751.751 0 0 1 .018-1.042.751.751 0 0 1 1.042-.018L6 10.94l6.72-6.72a.75.75 0 0 1 1.06 0Z"></path></svg>
			</button>
		</div>
		<div class="code-wrap">
			<pre>drip tcp 5432</pre>
			<button class="copy-btn" onclick="copy(this)">
				<svg class="copy" viewBox="0 0 16 16" fill="currentColor"><path d="M0 6.75C0 5.784.784 5 1.75 5h1.5a.75.75 0 0 1 0 1.5h-1.5a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-1.5a.75.75 0 0 1 1.5 0v1.5A1.75 1.75 0 0 1 9.25 16h-7.5A1.75 1.75 0 0 1 0 14.25Z"></path><path d="M5 1.75C5 .784 5.784 0 6.75 0h7.5C15.216 0 16 .784 16 1.75v7.5A1.75 1.75 0 0 1 14.25 11h-7.5A1.75 1.75 0 0 1 5 9.25Zm1.75-.25a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-7.5a.25.25 0 0 0-.25-.25Z"></path></svg>
				<svg class="check" viewBox="0 0 16 16" fill="currentColor"><path d="M13.78 4.22a.75.75 0 0 1 0 1.06l-7.25 7.25a.75.75 0 0 1-1.06 0L2.22 9.28a.751.751 0 0 1 .018-1.042.751.751 0 0 1 1.042-.018L6 10.94l6.72-6.72a.75.75 0 0 1 1.06 0Z"></path></svg>
			</button>
		</div>

		<div class="links">
			<a href="/health">Health Check</a>
			<a href="/stats">Statistics</a>
			<a href="/metrics">Prometheus Metrics</a>
		</div>

		<footer>
			<a href="https://github.com/Gouryella/drip" target="_blank">GitHub</a>
		</footer>
	</div>
	<script>
	function copy(btn) {
		const text = btn.previousElementSibling.textContent;
		navigator.clipboard.writeText(text).then(() => {
			btn.classList.add('copied');
			setTimeout(() => { btn.classList.remove('copied'); }, 2000);
		});
	}
	</script>
</body>
</html>`

	httputil.WriteHTML(w, []byte(html))
}

func (h *Handler) serveTunnelNotFound(w http.ResponseWriter, r *http.Request) {
	html := `<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8" />
	<meta name="viewport" content="width=device-width, initial-scale=1.0" />
	<title>404 - Tunnel Not Found</title>
	` + faviconLink + `
	<style>
		* { margin: 0; padding: 0; box-sizing: border-box; }
		body {
			font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
			background: #fff;
			color: #24292f;
			line-height: 1.6;
		}
		.container { max-width: 720px; margin: 0 auto; padding: 48px 24px; }
		header { margin-bottom: 48px; }
		h1 { font-size: 28px; font-weight: 600; margin-bottom: 8px; }
		h1 span { margin-right: 8px; }
		.desc { color: #57606a; font-size: 16px; }
		p { margin-bottom: 16px; }
		.info-box {
			background: #f6f8fa;
			border: 1px solid #d0d7de;
			border-radius: 6px;
			padding: 16px;
			margin: 24px 0;
		}
		.info-box ul {
			margin: 12px 0 0 20px;
			color: #57606a;
		}
		.info-box li { margin-bottom: 8px; }
		footer { margin-top: 48px; padding-top: 24px; border-top: 1px solid #d0d7de; }
		footer a { color: #57606a; text-decoration: none; font-size: 14px; }
		footer a:hover { color: #0969da; }
	</style>
</head>
<body>
	<div class="container">
		<header>
			<h1><span>🔍</span>Tunnel Not Found</h1>
			<p class="desc">The requested tunnel does not exist or has been closed.</p>
		</header>

		<div class="info-box">
			<p>This could happen because:</p>
			<ul>
				<li>The tunnel was never created</li>
				<li>The tunnel has been closed by the owner</li>
				<li>The tunnel URL is incorrect</li>
			</ul>
		</div>

		<p>If you are the tunnel owner, please restart your tunnel client.</p>

		<footer>
			<a href="https://github.com/Gouryella/drip" target="_blank">GitHub</a>
		</footer>
	</div>
</body>
</html>`

	httputil.WriteHTMLWithStatus(w, []byte(html), http.StatusNotFound)
}

func (h *Handler) serveHealth(w http.ResponseWriter, r *http.Request) {
	health := map[string]interface{}{
		"status":         "ok",
		"active_tunnels": h.manager.Count(),
		"timestamp":      time.Now().Unix(),
	}

	data, err := json.Marshal(health)
	if err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}

	httputil.WriteJSON(w, data)
}

func (h *Handler) serveStats(w http.ResponseWriter, r *http.Request) {
	if !h.validateMetricsAuth(w, r, "stats") {
		return
	}

	connections := h.manager.List()

	tunnelStats := make([]map[string]interface{}, 0, len(connections))
	for _, conn := range connections {
		if conn == nil {
			continue
		}
		tunnelStats = append(tunnelStats, map[string]interface{}{
			"subdomain":          conn.Subdomain,
			"tunnel_type":        string(conn.GetTunnelType()),
			"last_active":        conn.LastActive.Unix(),
			"bytes_in":           conn.GetBytesIn(),
			"bytes_out":          conn.GetBytesOut(),
			"active_connections": conn.GetActiveConnections(),
			"total_bytes":        conn.GetBytesIn() + conn.GetBytesOut(),
		})
	}

	stats := map[string]interface{}{
		"total_tunnels": len(tunnelStats),
		"tunnels":       tunnelStats,
	}

	data, err := json.Marshal(stats)
	if err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}

	httputil.WriteJSON(w, data)
}

func (h *Handler) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if !h.validateMetricsAuth(w, r, "metrics") {
		return
	}

	promhttp.Handler().ServeHTTP(w, r)
}

func (h *Handler) serveDiscovery(w http.ResponseWriter, r *http.Request) {
	transports := h.allowedTransports
	if len(transports) == 0 {
		transports = []string{"tcp", "wss"}
	}

	tunnelTypes := h.allowedTunnelTypes
	if len(tunnelTypes) == 0 {
		tunnelTypes = []string{"http", "https", "tcp"}
	}

	response := map[string]interface{}{
		"transports":   transports,
		"tunnel_types": tunnelTypes,
		"preferred":    h.GetPreferredTransport(),
		"version":      "1",
	}

	data, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Cache-Control", "no-cache")
	httputil.WriteJSON(w, data)
}
