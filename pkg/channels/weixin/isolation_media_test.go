package weixin

import (
	"os"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
)

func TestWeixinIsolationInboundMediaRetainsOwner(t *testing.T) {
	store := media.NewFileMediaStore()
	t.Cleanup(func() { _ = store.ReleaseAll("weixin:user:message") })
	var owners []string
	for _, channel := range []string{"WeixinBusiness", "weixinbusiness"} {
		bc := &config.Channel{Type: config.ChannelWeixin}
		bc.SetName(channel)
		settings := &config.WeixinSettings{IsolateAccounts: true, AccountID: "bot"}
		settings.SetToken("token")
		ch, err := NewWeixinChannel(bc, settings, nil)
		if err != nil {
			t.Fatal(err)
		}
		ch.accountKey = "alice"
		ch.SetMediaStore(store)
		ref, err := ch.storeInboundBytes("user", "message", "report.txt", "text/plain", []byte("本人附件"))
		if err != nil {
			t.Fatal(err)
		}
		path, meta, err := store.ResolveWithMeta(ref)
		if err != nil {
			t.Fatal(err)
		}
		want := config.WeixinIsolationAgentID(channel, "alice", "bot", "", "token")
		if meta.OwnerAgentID != want || meta.ContentType != "text/plain" {
			t.Fatalf("附件归属或类型不正确: %+v", meta)
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != "本人附件" {
			t.Fatalf("附件内容丢失: %q, %v", data, err)
		}
		owners = append(owners, meta.OwnerAgentID)
	}
	if owners[0] == owners[1] {
		t.Fatal("不同配置键的附件不能共用归属")
	}
}
