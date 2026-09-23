package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// The shell and every embedded asset share one content fingerprint. An open
// tab keeps its loaded release; its next full navigation requests matching new
// URLs even if a browser/proxy incorrectly retained an old unversioned asset.
var versionedWebUIPage = sync.OnceValues(func() ([]byte, error) {
	page, err := assets.ReadFile("static/webui.html")
	if err != nil {
		return nil, err
	}
	names := []string{"webui.css", "webui-format.js", "webui-commands.js", "webui-command-ui.js", "webui-notifications.js", "webui.js"}
	hash := sha256.New()
	_, _ = hash.Write(page)
	for _, name := range names {
		data, err := assets.ReadFile("static/" + name)
		if err != nil {
			return nil, err
		}
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(data)
	}
	version := hex.EncodeToString(hash.Sum(nil))[:20]
	for _, name := range names {
		path := "/tgw/webui/static/" + name
		page = bytes.ReplaceAll(page, []byte(path+`"`), []byte(path+"?v="+version+`"`))
	}
	return page, nil
})
