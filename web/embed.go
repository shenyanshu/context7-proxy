// Package web 内嵌前端构建产物（dist），由 server 层挂载为 /admin 静态资源。
// 只允许构建后的 dist 进入二进制：源码 src 不内嵌，避免体积膨胀与泄漏源文件。
package web

import "embed"

// DistFS 是前端构建产物的只读文件系统，根目录为 dist。
//
//go:embed all:dist
var DistFS embed.FS
