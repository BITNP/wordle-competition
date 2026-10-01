// Package web 把前端页面与题库整个打进二进制（go:embed），是运行时的唯一静态资源来源。
//
// 目录布局就是 URL 布局，所以不需要任何额外路由：
//
//	index.html + app.js + style.css  选手页（/）
//	common.js + base.css             共享脚本与主题
//	admin.html                       管理面板（/admin）
//	api.html                         选手 API 说明页（/api.html）
//	words/answers.txt、words/valid.txt  题库（/words/…）
//
// 单独一个包是因为 go:embed 不能引用上层目录；后端用 web.FS 读取，
// 词表由 words.Load(fs.Sub(web.FS, "words")) 加载，与 /words/ 下发的是同一份文件。
package web

import "embed"

//go:embed index.html app.js common.js base.css style.css admin.html api.html words/answers.txt words/valid.txt
var FS embed.FS
