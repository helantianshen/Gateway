// Package policy 提供按路由编译的策略链；当前装配的策略链为空
package policy

import (
	"fmt"
	"net/http"

	"github.com/helantianshen/gateway/internal/dataplane/response"
)

// Middleware 把一个策略包装到 next 外层。实现必须在创建后并发安全
type Middleware interface {
	Wrap(next http.Handler) http.Handler
}

// CompiledChain 是启动期完成包装、请求期只执行的不可变 Handler
type CompiledChain struct {
	handler http.Handler
}

// Compile 按声明顺序编译策略；middlewares[0] 最先看到请求
func Compile(terminal http.Handler, middlewares []Middleware) (*CompiledChain, error) {
	if terminal == nil {
		return nil, fmt.Errorf("编译路由策略链失败: terminal handler 不能为空")
	}

	handler := terminal
	for index := len(middlewares) - 1; index >= 0; index-- {
		if middlewares[index] == nil {
			return nil, fmt.Errorf("编译路由策略链失败: middlewares[%d] 不能为空", index)
		}
		handler = middlewares[index].Wrap(handler)
		if handler == nil {
			return nil, fmt.Errorf("编译路由策略链失败: middlewares[%d] 返回 nil handler", index)
		}
	}
	return &CompiledChain{handler: handler}, nil
}

// ServeHTTP 执行已编译策略链
func (c *CompiledChain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if c == nil || c.handler == nil {
		response.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "route policy chain unavailable")
		return
	}
	c.handler.ServeHTTP(w, r)
}
