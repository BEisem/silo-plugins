package main

import (
	"context"
	"encoding/json"
	"html"
	"net/url"
	"regexp"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// Links in emails go through a small public page that opens the item in the
// Silo app when it is installed, and on the web otherwise. Email clients
// don't make silo:// links clickable, and Silo's self-hosted domains can't
// carry the apps' universal links, so an https hop is the only way in.
//
// The apps route silo://item/<content id> to the item's page (silo-android
// ContentDeepLinkRoutes.kt, silo-apple ContentView.handleDeepLink).

const androidPackage = "org.siloserver.silo"

var contentIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:~-]{1,200}$`)

// link returns the URL an email uses for a catalog item.
func (env RenderEnv) link(id string) string {
	if env.Settings.Theme.OpenInApp && env.PluginBase != "" && contentIDPattern.MatchString(id) {
		return env.PluginBase + "/public/open?id=" + url.QueryEscape(id)
	}
	return itemURL(env.PublicURL, id)
}

// serveOpen answers /public/open with the app-or-web hand-off page.
func (a *App) serveOpen(ctx context.Context, req request) *pluginv1.HandleHTTPResponse {
	id := req.q("id")
	ep := a.endpoints(ctx)
	if !contentIDPattern.MatchString(id) || ep.public == "" {
		return simplePage(400, "Link not valid", "This link doesn't point to anything on the server.", "")
	}
	web := itemURL(ep.public, id)
	app := "silo://item/" + url.PathEscape(id)
	intent := "intent://item/" + url.PathEscape(id) + "#Intent;scheme=silo;package=" + androidPackage +
		";S.browser_fallback_url=" + url.QueryEscape(web) + ";end"
	js := func(s string) string {
		b, _ := json.Marshal(s) // escapes <, > and & for a script context
		return string(b)
	}
	buttons := `<p><a class="btn" href="` + html.EscapeString(app) + `">Open in the Silo app</a> <a class="btn alt" href="` +
		html.EscapeString(web) + `">Open in browser</a></p>
<script>
(function () {
  var web = ` + js(web) + `, app = ` + js(app) + `, intent = ` + js(intent) + `;
  var ua = navigator.userAgent || "";
  if (/android/i.test(ua)) {
    window.location.replace(intent);
  } else if (/iphone|ipad|ipod/i.test(ua) || (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1)) {
    var timer = setTimeout(function () { window.location.replace(web); }, 1600);
    document.addEventListener("visibilitychange", function () { if (document.hidden) clearTimeout(timer); });
    window.location.href = app;
  } else {
    window.location.replace(web);
  }
})();
</script>`
	resp := simplePage(200, "Opening in Silo…", "If nothing happens, choose where to open it.", buttons)
	resp.Headers["Cache-Control"] = "no-store"
	return resp
}
