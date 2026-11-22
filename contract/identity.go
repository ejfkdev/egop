package contract

// 调用方身份透传:宿主在执行 Surface.Exec/Op 前把插件 id 放进 ctx,供装配层
// 做"按插件隔离"的解析(如插件私有 bin 目录、PATH 前缀)。这是宿主内部
// 装配约定,不改变 Surface 接口/ABI。
import "context"

type pluginIDKey struct{}

// WithPluginID 返回携带调用方插件 id 的 ctx(宿主 surface 自动注入)。
func WithPluginID(ctx context.Context, id string) context.Context {
	if id == "" || ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, pluginIDKey{}, id)
}

// PluginIDFromContext 读取调用方插件 id(空=非插件面调用,如宿主内部)。
func PluginIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(pluginIDKey{}).(string)
	return id
}
