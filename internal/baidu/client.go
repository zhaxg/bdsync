// Package baidu 封装 qjfoidnh/BaiduPCS-Go 的 baidupcs 核心：登录(cookie) + 上传(秒传V2/分片/合并)。
//
// 只用其可导出的核心 API，不引入其 CLI/配置/内部包。
// 上传流程与上游 internal/pcsfunctions/pcsupload 对齐：
//
//	秒传V2（命中即完成）→ 未命中拿 uploadid → 逐分片 UploadTmpFile → UploadCreateSuperFile 合并
package baidu

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/qjfoidnh/BaiduPCS-Go/baidupcs"
	"github.com/qjfoidnh/BaiduPCS-Go/baidupcs/pcserror"
)

// DefaultAppID — fork 的 pcsconfig 默认值（实测 250528 会触发 31030 pcs token not exist）。
const DefaultAppID = 266719

// Policy — 同名文件处理策略（fork 里是 var 不是 const）。备份场景用 overwrite。
var (
	PolicyOverwrite = baidupcs.OverWritePolicy
	PolicySkip      = baidupcs.SkipPolicy
	PolicyRsync     = baidupcs.RsyncPolicy
)

// Client — 单账号会话（cookie 认证）。
type Client struct {
	pcs       *baidupcs.BaiduPCS
	cookieStr string

	ukMu sync.Mutex
	uk   int64
	ukOK bool
}

// New — cookieStr 形如 "BDUSS=xxx; STOKEN=yyy"（STOKEN 可选，部分 pan 接口需要）。
func New(appID int, cookieStr string) (*Client, error) {
	// 统一过一遍规范化：只留 BDUSS/STOKEN/BAIDUID，并清掉 net/http 会告警丢弃的非法
	// 字节（老版本把浏览器整串 cookie 存进 credentials.dat，其中 RT 的值带引号，
	// 每次请求都会在日志里刷一行 invalid byte '"' in Cookie.Value）。
	cookieStr = SanitizeCookie(cookieStr)
	if !strings.Contains(cookieStr, "BDUSS=") {
		return nil, errors.New("cookie 缺少 BDUSS（环境变量 BDSYNC_COOKIE，形如 \"BDUSS=xxx; STOKEN=yyy\"）")
	}
	if appID == 0 {
		appID = DefaultAppID
	}
	return &Client{
		pcs:       baidupcs.NewPCSWithCookieStr(appID, cookieStr),
		cookieStr: cookieStr,
	}, nil
}

// UK — 秒传V2 的偏移计算需要用户 uk（SDK 内部带 24h 缓存）。
func (c *Client) UK() (int64, error) {
	c.ukMu.Lock()
	defer c.ukMu.Unlock()
	if c.ukOK {
		return c.uk, nil
	}
	uk, perr := c.pcs.CacheUK()
	if perr != nil {
		return 0, fmt.Errorf("获取用户 uk 失败（确认 BDUSS/STOKEN 有效）: %w", perr)
	}
	c.uk, c.ukOK = uk, true
	return uk, nil
}

// RapidResult — 秒传V2 响应中的可用信息（含 uploadid，秒传未命中时分片要用）。
type RapidResult struct {
	ReturnType int
	UploadID   string
	FsID       int64
	Path       string
	MD5        string
	Size       int64
	Mtime      int64
}

// RapidUploadV2 — 秒传（design §7 步骤3）。uploadID 首次传 ""；
// 秒传未命中时（31079 等）响应里的 UploadID 用于后续分片上传。
// 返回值：结果可能携带 UploadID，即使 err != nil 也可用。
func (c *Client) RapidUploadV2(targetPath, policy, uploadID, contentMD5, sliceMD5, b64Content, crc32 string,
	offset, dataLen, totalSize, dataTime int64, blockListMD5 []string) (*RapidResult, error) {

	perr, res := c.pcs.RapidUpload(targetPath, policy, uploadID, contentMD5, sliceMD5, b64Content, crc32,
		offset, dataLen, totalSize, dataTime, blockListMD5)

	out := &RapidResult{
		ReturnType: res.ReturnType,
		UploadID:   res.UploadID,
		FsID:       res.FsID,
		Path:       res.Path,
		MD5:        res.MD5,
		Size:       res.Size,
		Mtime:      res.Mtime,
	}
	if perr != nil {
		return out, perr
	}
	return out, nil
}

// FakeRapidUpload — 不做秒传，仅为拿 uploadid（断点续传/跳过秒传时用）。
func (c *Client) FakeRapidUpload(targetPath, policy string, length int64) (string, error) {
	perr, res := c.pcs.FakeRapidUpload(targetPath, policy, length)
	if perr != nil {
		return res.UploadID, perr
	}
	return res.UploadID, nil
}

var uploadTransport = &http.Transport{
	MaxIdleConns:          8,
	MaxIdleConnsPerHost:   4,
	TLSHandshakeTimeout:   30_000_000_000,  // 30s
	ResponseHeaderTimeout: 120_000_000_000, // 120s
	IdleConnTimeout:       90_000_000_000,  // 90s
	ForceAttemptHTTP2:     true,
}

// UploadSingle — 单请求直传（老式 method=upload）。0 字节文件等无法走分片/秒传的场景用。
func (c *Client) UploadSingle(targetPath, policy string, r io.Reader, size int64) error {
	rc, perr := c.pcs.PrepareUpload(policy, targetPath, c.blockUploadFunc(r, size))
	if perr != nil {
		return perr
	}
	defer rc.Close()
	var resp struct {
		Path      string `json:"path"`
		ErrorCode int    `json:"error_code"`
		ErrMsg    string `json:"errmsg"`
	}
	body, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("解析直传响应失败: %w (body=%.200s)", err, body)
	}
	if resp.ErrorCode != 0 {
		return fmt.Errorf("直传失败: code=%d %s", resp.ErrorCode, resp.ErrMsg)
	}
	if resp.Path == "" {
		return fmt.Errorf("直传响应缺少 path (body=%.200s)", body)
	}
	return nil
}

// UploadBlock — 上传一个分片（与上游 PCSUpload.TmpFile 同法：multipart 字段名 uploadedfile）。
// data 为该分片的数据源，size 为分片字节数。返回服务端给出的分片 md5。
//
// 注意：服务端拒绝 chunked 编码（31211 transfer-encoding can't be chunked），
// 必须带 Content-Length —— 先把整个分片（典型 4MB，上限 64MB）读进内存再发。
func (c *Client) UploadBlock(targetPath, uploadID string, partseq int, partOffset int64, data io.Reader, size int64) (string, error) {
	md5sum, perr := c.pcs.UploadTmpFile(uploadID, targetPath, partseq, partOffset, c.blockUploadFunc(data, size))
	if perr != nil {
		return "", perr
	}
	return md5sum, nil
}

// blockUploadFunc — SDK 的 UploadFunc 回调：自行完成 multipart POST。
//   - 先构建完整 body（带 Content-Length，服务端不接受 chunked）
//   - SDK 生成的 URL 是 http://，直接升级 https 并禁止重定向（body 不可重放）
//   - cookie 手工附加
func (c *Client) blockUploadFunc(r io.Reader, size int64) baidupcs.UploadFunc {
	return func(uploadURL string, _ http.CookieJar) (*http.Response, error) {
		uploadURL = strings.Replace(uploadURL, "http://", "https://", 1)

		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, err := mw.CreateFormFile("uploadedfile", "")
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(part, io.LimitReader(r, size)); err != nil {
			return nil, err
		}
		if err := mw.Close(); err != nil {
			return nil, err
		}

		req, err := http.NewRequest(http.MethodPost, uploadURL, bytes.NewReader(buf.Bytes()))
		if err != nil {
			return nil, err
		}
		req.ContentLength = int64(buf.Len())
		req.Header.Set("Content-Type", mw.FormDataContentType())
		for _, ck := range ParseCookies(c.cookieStr) {
			req.AddCookie(ck)
		}

		hc := &http.Client{
			Transport: uploadTransport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse // 3xx 当作响应交给 SDK 判错
			},
		}
		return hc.Do(req)
	}
}

// CreateSuperFile — 合并分片为最终文件（design §7 步骤5）。
func (c *Client) CreateSuperFile(targetPath, uploadID, policy string, fileSize int64, checksumMap map[int]string) error {
	if perr := c.pcs.UploadCreateSuperFile(uploadID, policy, fileSize, targetPath, checksumMap); perr != nil {
		return perr
	}
	return nil
}

// Move — 远端移动文件/目录（pan API，服务器端改位置：零带宽、不下载不重传）。
// 镜像模式的归档（历史+主树删除一步完成）与改名同步都靠它。
func (c *Client) Move(from, to string) error {
	if perr := c.pcs.Move(&baidupcs.CpMvJSON{From: from, To: to}); perr != nil {
		return perr
	}
	return nil
}

// RemoteMissing — 判断错误是否为"远端路径不存在"（31066 / -9）。
func RemoteMissing(err error) bool {
	pe, ok := AsPCSError(err)
	if !ok {
		return false
	}
	code := pe.GetRemoteErrCode()
	return code == 31066 || code == -9
}

// Mkdir — 创建远端目录；已存在(31061)视为成功。
func (c *Client) Mkdir(p string) error {
	perr := c.pcs.Mkdir(p)
	if perr == nil {
		return nil
	}
	if pe, ok := AsPCSError(perr); ok && pe.GetRemoteErrCode() == 31061 {
		return nil
	}
	return perr
}

// FileInfo — 远端文件元信息（写回索引用）。
type FileInfo struct {
	FsID  int64
	Path  string
	MD5   string
	Size  int64
	Mtime int64
	Isdir bool
}

// DownloadAndHash — 下载远端文件并计算 md5（verify 校验的底层能力，design §5 verify）。
//   - 先尝试一次性下载（整 Range，CDN 对小文件直接回 200/206）
//   - 大文件整 Range 可能被拒（403 hitcode:104）→ 降级为 256KB 窗口逐段下载拼接哈希
func (c *Client) DownloadAndHash(remotePath string, size int64) (string, int64, error) {
	info, perr := c.pcs.LocateDownload(remotePath)
	if perr != nil {
		return "", 0, perr
	}
	u := info.SingleURL(true)
	if u == nil {
		return "", 0, errors.New("无可用下载链接")
	}
	link := u.String()

	// 1) 整文件一次性下载
	h := md5.New()
	if n, err := c.streamRange(link, 0, size-1, size, h); err == nil {
		return hex.EncodeToString(h.Sum(nil)), n, nil
	}

	// 2) 降级：256KB 窗口逐段
	const window = 256 * 1024
	h.Reset()
	var total int64
	for off := int64(0); size <= 0 || off < size; off += window {
		end := off + window
		if size > 0 && end > size {
			end = size
		}
		n, err := c.streamRange(link, off, end-1, size, h)
		if err != nil {
			return "", total, fmt.Errorf("分段下载失败(偏移 %d): %w", off, err)
		}
		total += n
		if n == 0 {
			break
		}
	}
	if size > 0 && total != size {
		return "", total, fmt.Errorf("下载不完整: %d/%d", total, size)
	}
	return hex.EncodeToString(h.Sum(nil)), total, nil
}

// streamRange — 请求 [from,to]（含端点）并写入 h，返回字节数。接受 200/206。
func (c *Client) streamRange(link string, from, to, size int64, h hash.Hash) (int64, error) {
	req, err := http.NewRequest(http.MethodGet, link, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", baidupcs.NetdiskUA)
	if size > 0 || to >= from {
		if to >= from {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to))
		}
	}
	for _, ck := range ParseCookies(c.cookieStr) {
		req.AddCookie(ck)
	}
	hc := &http.Client{Transport: uploadTransport, Timeout: 30 * time.Minute}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		if h == nil {
			h = md5.New()
		}
		n, err := io.Copy(h, resp.Body)
		return n, err
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("HTTP %d %.200s", resp.StatusCode, body)
	}
}

// FixMD5 — 分片上传后服务端 md5 元数据可能与真实内容不一致（百度已知问题）。
// fork 的做法：定位下载取真实内容头 → 与服务端存储对比 → 用秒传原理重写元数据。
// 顺带构成一次轻量内容校验（design §5 verify 的雏形）。
func (c *Client) FixMD5(remotePath string) error {
	if perr := c.pcs.FixMD5(remotePath); perr != nil {
		return perr
	}
	return nil
}

// Remove — 远端删除（目录为服务器端递归删除）。仅由历史清理使用；
// 正常的"删除传播"一律走 Move 归档，绝不直接删（design镜像）。
func (c *Client) Remove(p string) error {
	if perr := c.pcs.Remove(p); perr != nil {
		return perr
	}
	return nil
}

// List — 列出远端目录内容（看板/验证用）。
func (c *Client) List(remotePath string) ([]FileInfo, error) {
	fdl, perr := c.pcs.FilesDirectoriesList(remotePath, baidupcs.DefaultOrderOptions)
	if perr != nil {
		return nil, perr
	}
	out := make([]FileInfo, 0, len(fdl))
	for _, fd := range fdl {
		out = append(out, FileInfo{
			FsID:  fd.FsID,
			Path:  fd.Path,
			MD5:   fd.MD5,
			Size:  fd.Size,
			Mtime: fd.Mtime,
			Isdir: fd.Isdir,
		})
	}
	return out, nil
}

// Meta — 查询远端文件元信息。
func (c *Client) Meta(p string) (*FileInfo, error) {
	fd, perr := c.pcs.FilesDirectoriesMeta(p)
	if perr != nil {
		return nil, perr
	}
	return &FileInfo{
		FsID:  fd.FsID,
		Path:  fd.Path,
		MD5:   fd.MD5,
		Size:  fd.Size,
		Mtime: fd.Mtime,
		Isdir: fd.Isdir,
	}, nil
}

// AsPCSError — 从 error 取出 SDK 的 pcserror.Error（design §12：接口不是 error，需断言）。
func AsPCSError(err error) (pcserror.Error, bool) {
	if err == nil {
		return nil, false
	}
	pe, ok := err.(pcserror.Error)
	return pe, ok
}

// ParseCookies — "A=1; B=2" → []*http.Cookie。
func ParseCookies(s string) []*http.Cookie {
	var out []*http.Cookie
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		out = append(out, &http.Cookie{Name: strings.TrimSpace(k), Value: strings.TrimSpace(v)})
	}
	return out
}
