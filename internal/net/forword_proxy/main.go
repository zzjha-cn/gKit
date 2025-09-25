package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var (
	addr     = flag.String("addr", ":8888", "代理服务器监听地址")
	certFile = flag.String("cert", "cert.pem", "TLS证书文件路径")
	keyFile  = flag.String("key", "key.pem", "TLS私钥文件路径")
	verbose  = flag.Bool("v", false, "启用详细日志")
	mitm     = flag.Bool("mitm", false, "启用MITM模式拦截HTTPS流量")
)

func main() {
	flag.Parse()

	// 创建日志记录器
	logger := log.New(os.Stdout, "[PROXY] ", log.LstdFlags|log.Lshortfile)

	// 创建代理服务器
	var handler http.Handler
	if *mitm {
		// 启用MITM模式
		mitmProxy, err := NewMITMProxy(&ProxyConfig{
			Logger:   logger,
			CertFile: *certFile,
			KeyFile:  *keyFile,
			Verbose:  *verbose,
		}, true)
		if err != nil {
			logger.Fatalf("创建MITM代理服务器失败: %v", err)
		}
		handler = mitmProxy
		logger.Printf("MITM模式已启用 - 将拦截HTTPS流量")
	} else {
		// 普通代理模式
		proxy, err := NewProxy(&ProxyConfig{
			Logger:   logger,
			CertFile: *certFile,
			KeyFile:  *keyFile,
			Verbose:  *verbose,
		})
		if err != nil {
			logger.Fatalf("创建代理服务器失败: %v", err)
		}
		handler = proxy
	}

	// 创建HTTP服务器
	server := &http.Server{
		Addr:         *addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// 启动服务器
	go func() {
		logger.Printf("代理服务器启动在 %s", *addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("服务器启动失败: %v", err)
		}
	}()

	// 等待中断信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Println("正在关闭代理服务器...")

	// 优雅关闭
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Printf("服务器关闭失败: %v", err)
	}

	// 关闭代理资源
	if closer, ok := handler.(interface{ Close() error }); ok {
		closer.Close()
	}
	logger.Println("代理服务器已关闭")
}
