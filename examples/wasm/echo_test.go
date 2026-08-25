// examples/wasm 最小插件冒烟:echo.egop.wasm 是 echo.wat 的编译产物(双双入库,
// 改 wat 后用 wabt 的 wat2wasm 重编:见本目录 README)。
package examplewasm

import (
	"context"
	"os"
	"testing"

	"github.com/ejfkdev/egop/loader/wasm"
)

func TestMinimalEchoLoads(t *testing.T) {
	b, err := os.ReadFile("echo.egop.wasm")
	if err != nil {
		t.Fatal(err)
	}
	p, err := wasm.LoadFS(context.Background(), b, "echo.egop.wasm", wasm.Options{})
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	defer p.Close(context.Background())
	if p.Meta().ID != "echo" || p.Meta().Name != "Echo" {
		t.Fatalf("meta = %+v", p.Meta())
	}
}
