// Package deployscan 用测试的形式扫描部署脚本与文档的自我一致性。
//
// 它不是产品代码，存在的唯一理由是**这类错误只有真机安装时才会炸**：
//
//	install: cannot stat 'deploy/tmpfiles-shengyu.conf': No such file or directory
//
// 那是一次品牌改名留下的：文件被重命名为 tmpfiles-shengyu-edgelink.conf，
// 脚本里的引用却还写着旧名。同样的改名还把
// `systemctl enable --now ... shengyu-edgelink-server` 截断成了
// `shengyu-edgelink`（一个并不存在的单元），直到真机执行才暴露。
//
// 编译期查不出 shell 脚本里的字符串引用，所以只能靠测试守住：
// 引用的文件必须真的存在，systemctl 引用的单元名必须是完整的两个正式名之一。
package deployscan
