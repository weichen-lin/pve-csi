# pve-csi

目前提供 CSI `CreateVolume`、`ValidateVolumeCapabilities` 與 Identity 服務，使用 Unix socket。支援 PVE LVM-thin／raw 磁碟；尚未提供刪碟、接到 VM、掛載及完整 PVC 自動供應。

## DDD 分層

| 目錄 | 責任 |
|---|---|
| `cmd/pve-csi` | 啟動、設定與組裝 |
| `internal/domain` | 磁碟、容量、識別與儲存池規則 |
| `internal/volume` | 建碟與查詢流程、儲存介面與權限檢查 |
| `internal/csi` | CSI gRPC 服務、請求驗證與錯誤轉換 |
| `internal/pve` | HTTPS client、PVE API 與儲存介面實作 |

`volume` 只依賴 `domain`；`csi`、`pve` 接到 `volume` 的介面。`domain` 不依賴 CSI 或 PVE。

建碟：查有效權限與池狀態 → 檢查可讀 → 查既有磁碟 → 已存在則回傳；否則檢查建碟權限與空間 → 建碟一次 → 讀回確認。

儲存池由每筆 CSI 請求的 `parameters.storage` 指定。`CheckReadable()`／`CheckAllocatable()` 位於 `volume`；`Pool.CheckReady()`／`CheckCapacity()` 位於 `domain`。檢查不會保留空間；結果未知時必須用原名稱及原設定重試。

## 啟動

| 環境變數 | 用途 |
|---|---|
| `PVE_ENDPOINT` | PVE HTTPS 位址，主機名稱／IP 必須符合憑證 |
| `PVE_TOKEN_ID`、`PVE_TOKEN_SECRET` | API token |
| `PVE_CA_FILE` | 自訂 CA PEM 路徑；省略則使用系統信任憑證 |
| `PVE_NODE` | PVE 主機名稱 |
| `PVE_OWNER_VMID` | 預留的磁碟歸屬編號，100～999999999；不是接碟目標 |
| `CSI_ENDPOINT` | 選填，預設 `unix:///tmp/pve-csi.sock` |

設定完成後：

```sh
rtk proxy env GOTOOLCHAIN=go1.27.2 go build -o /tmp/pve-csi ./cmd/pve-csi
rtk proxy /tmp/pve-csi controller
```

正式程式只有 `controller` 指令。建碟要求透過 CSI gRPC 傳入。
