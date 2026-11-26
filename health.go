package main

import (
	"fmt"
	"runtime"
	"time"
)

type healthItem struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Status string `json:"status"` // "ok" | "warn" | "fail"
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

type healthReport struct {
	Overall   string       `json:"overall"` // "ok" | "warn" | "fail"
	CheckedAt int64        `json:"checkedAt"`
	Summary   string       `json:"summary"`
	BadCache  int64        `json:"badCache"`
	Items     []healthItem `json:"items"`
}

func (a *app) runHealthCheck() healthReport {
	a.invalidateSystem()
	sys := a.system()
	rep := healthReport{
		Overall:   "ok",
		CheckedAt: time.Now().Unix(),
		Items:     make([]healthItem, 0, 6),
	}

	raiseOverall := func(st string) {
		switch st {
		case "fail":
			rep.Overall = "fail"
		case "warn":
			if rep.Overall == "ok" {
				rep.Overall = "warn"
			}
		}
	}

	// 1. 双端口物理隔离
	portItem := healthItem{
		Key:    "ports",
		Name:   "双端口物理隔离",
		Status: "ok",
	}
	if a.isDualPort() {
		lanTxt := "仅本机回环"
		if a.p.allowLAN.Load() {
			lanTxt = "局域网已开放（API 仍锁定本机）"
		}
		portItem.Detail = fmt.Sprintf("数据代理 %s 与控制面板 %s 已物理分离（%s）", a.p.addr, a.controlAddr(), lanTxt)
	} else {
		portItem.Status = "warn"
		portItem.Detail = fmt.Sprintf("当前运行在单端口兼容模式（%s）", a.p.addr)
		portItem.Hint = "建议不要传相同的 -admin-addr，保留自动双端口隔离。"
	}
	raiseOverall(portItem.Status)
	rep.Items = append(rep.Items, portItem)

	// 2. 本机根证书
	certItem := healthItem{
		Key:  "cert",
		Name: "本机 HTTPS 根证书",
	}
	switch {
	case runtime.GOOS != "windows":
		certItem.Status = "ok"
		certItem.Detail = "非 Windows 环境：请确认已在系统或浏览器信任 ca.crt"
	case sys.CertTrusted && sys.OldCerts > 0:
		certItem.Status = "warn"
		certItem.Detail = fmt.Sprintf("当前根证书已受信任，但检测到 %d 张旧版本残留证书", sys.OldCerts)
		certItem.Hint = "建议在「本机证书」中清理旧证书，保持证书库整洁。"
	case sys.CertTrusted:
		certItem.Status = "ok"
		certItem.Detail = "根证书已安装并受系统信任（仅限 *.akamaized.net，绝不触碰其他站点）"
	default:
		certItem.Status = "fail"
		certItem.Detail = "尚未安装或未信任 GBF-Dock 根证书"
		certItem.Hint = "点击仪表盘「本机证书 -> 安装」，弹窗选“是”，随后重启浏览器。"
	}
	raiseOverall(certItem.Status)
	rep.Items = append(rep.Items, certItem)

	// 3. 系统代理接管
	proxyItem := healthItem{
		Key:  "proxy",
		Name: "系统代理接管",
	}
	switch {
	case runtime.GOOS != "windows":
		proxyItem.Status = "ok"
		proxyItem.Detail = fmt.Sprintf("请将浏览器代理指向 %s", a.p.addr)
	case sys.LegacyPAC:
		proxyItem.Status = "warn"
		proxyItem.Detail = "检测到旧版 PAC 设置残留"
		proxyItem.Hint = "重新开关一次「代理接管」即可自动迁移为手动 HTTP 串联模式。"
	case sys.ProxyEnabled:
		proxyItem.Status = "ok"
		proxyItem.Detail = fmt.Sprintf("已通过手动 HTTP 代理串联接管系统流量（%s），未启用 PAC", a.p.addr)
	default:
		proxyItem.Status = "warn"
		proxyItem.Detail = "代理接管当前处于关闭（待机）状态"
		proxyItem.Hint = "开始游玩前请开启仪表盘上的「代理接管」开关。"
	}
	raiseOverall(proxyItem.Status)
	rep.Items = append(rep.Items, proxyItem)

	// 4. 上游回源通道
	mode, upSrc, tun, upAlive := a.upstreamSnapshot()
	upProxy := a.p.getUpstream()
	upItem := healthItem{
		Key:  "upstream",
		Name: "上游回源通道",
	}
	switch {
	case upProxy != "" && upAlive:
		upItem.Status = "ok"
		upItem.Detail = fmt.Sprintf("模式 %s · 已连通 %s（%s）", mode, upProxy, upSrc)
	case upProxy != "" && !upAlive:
		upItem.Status = "fail"
		upItem.Detail = fmt.Sprintf("上游节点 %s 当前无 TCP 响应（%s）", upProxy, upSrc)
		upItem.Hint = "请确认代理/加速器已启动，或点击「自动检测节点」切换可用端口。"
	case tun != "":
		upItem.Status = "ok"
		upItem.Detail = fmt.Sprintf("已识别加速器 / TUN 虚拟网卡（%s），走直连透明转发", tun)
	default:
		upItem.Status = "warn"
		upItem.Detail = fmt.Sprintf("当前为直连回源（%s）", upSrc)
		upItem.Hint = "若平时使用 Clash / v2rayN / 岛风GO，请先启动它们再点击「自动检测节点」。"
	}
	raiseOverall(upItem.Status)
	rep.Items = append(rep.Items, upItem)

	// 5. CDN 连通性与延迟
	cdnItem := healthItem{
		Key:  "cdn",
		Name: "GBF CDN 连通与延迟",
	}
	if ms, msg, err := a.pingUpstream(); err == nil {
		cdnItem.Status = "ok"
		cdnItem.Detail = msg
		if ms > 2500 {
			cdnItem.Status = "warn"
			cdnItem.Hint = "CDN 首包延迟较高，建议更换更稳定的游戏加速器节点。"
		}
	} else {
		cdnItem.Status = "fail"
		cdnItem.Detail = err.Error()
		cdnItem.Hint = "请检查上游节点或本机网络连接后再测速。"
	}
	raiseOverall(cdnItem.Status)
	rep.Items = append(rep.Items, cdnItem)

	// 6. 本地缓存目录与文件完整性
	checked, bad, badBytes := a.p.scanCacheHealth(true)
	rep.BadCache = bad
	ramItems, ramUsed, _ := a.p.ram.Stats()
	idxState := "索引建立中"
	if a.p.metaIdxReady.Load() {
		idxState = "索引已就绪"
	}
	cacheItem := healthItem{
		Key:  "cache",
		Name: "本地缓存完整性",
	}
	if bad > 0 {
		cacheItem.Status = "warn"
		cacheItem.Detail = fmt.Sprintf("共扫描 %d 个缓存文件（RAM 热缓存 %d 个，%s），发现 %d 个异常/损坏件（约 %.1f KB）", checked, ramItems, idxState, bad, float64(badBytes)/1024)
		cacheItem.Hint = "点击下方「立即清理坏档」可无痛剔除损坏文件，不影响正常缓存。"
	} else {
		cacheItem.Status = "ok"
		cacheItem.Detail = fmt.Sprintf("共扫描 %d 个缓存文件（目录：%s · RAM 热缓存 %d 个 / %.1f MB · %s），全部健康", checked, a.p.getCacheRoot(), ramItems, float64(ramUsed)/(1024*1024), idxState)
	}
	raiseOverall(cacheItem.Status)
	rep.Items = append(rep.Items, cacheItem)

	switch rep.Overall {
	case "ok":
		rep.Summary = "全部 6 项指标健康，当前可直接启动游戏畅享缓存加速。"
	case "warn":
		rep.Summary = "基础功能可用，但存在需要留意或可一键优化的提示项。"
	default:
		rep.Summary = "发现影响正常加速的异常项，请优先按下方建议修复。"
	}
	return rep
}
