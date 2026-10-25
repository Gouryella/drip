package httputil

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// CopyHeaders copies all headers from src to dst.
func CopyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// CleanHopByHopHeaders removes hop-by-hop headers that should not be forwarded.
func CleanHopByHopHeaders(headers http.Header) {
	if headers == nil {
		return
	}

	for _, connectionHeaders := range headers.Values("Connection") {
		for _, token := range strings.Split(connectionHeaders, ",") {
			if t := strings.TrimSpace(token); t != "" {
				headers.Del(t)
			}
		}
	}

	for _, key := range []string{
		"Connection",
		"Upgrade",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Proxy-Connection",
	} {
		headers.Del(key)
	}
}

// WriteProxyError writes an HTTP error response to the writer.
func WriteProxyError(w io.Writer, code int, msg string) {
	body := msg
	resp := &http.Response{
		StatusCode:    code,
		Status:        fmt.Sprintf("%d %s", code, http.StatusText(code)),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Close:         true,
	}
	resp.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	_ = resp.Write(w)
	_ = resp.Body.Close()
}

func WriteLocalServiceUnavailable(w io.Writer, localPort int) {
	html := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1.0" />
    <title>502 - Local Service Unavailable</title>
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
        code {
            background: #f6f8fa;
            border: 1px solid #d0d7de;
            border-radius: 4px;
            padding: 2px 6px;
            font-family: ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, monospace;
            font-size: 14px;
        }
        footer { margin-top: 48px; padding-top: 24px; border-top: 1px solid #d0d7de; }
        footer a { color: #57606a; text-decoration: none; font-size: 14px; }
        footer a:hover { color: #0969da; }
    </style>
</head>
<body>
    <div class="container">
        <header>
            <h1><span>⚠️</span>Local Service Unavailable</h1>
            <p class="desc">The tunnel is active, but the local service is not responding.</p>
        </header>

        <div class="info-box">
            <p>This could happen because:</p>
            <ul>
                <li>No service is running on port <code>%d</code></li>
                <li>The local service has crashed or stopped</li>
                <li>The service is still starting up</li>
            </ul>
        </div>

        <p>Please ensure your local service is running on port <code>%d</code> and try again.</p>

        <footer>
            <a href="https://github.com/Gouryella/drip" target="_blank">GitHub</a>
        </footer>
    </div>
</body>
</html>`, localPort, localPort)

	resp := &http.Response{
		StatusCode:    http.StatusBadGateway,
		Status:        "502 Bad Gateway",
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(html)),
		ContentLength: int64(len(html)),
		Close:         true,
	}
	resp.Header.Set("Content-Type", "text/html; charset=utf-8")
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(html)))
	_ = resp.Write(w)
	_ = resp.Body.Close()
}

// IsWebSocketUpgrade checks if the request is a WebSocket upgrade request.
func IsWebSocketUpgrade(req *http.Request) bool {
	return strings.EqualFold(req.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade")
}
