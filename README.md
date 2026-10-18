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

儲存池由每筆 CSI 請求的 `parameters.storage` 指定；磁碟歸屬編號讀取 `PVE_OWNER_VMID`。

## 建碟檢查與錯誤

| 順序／錯誤步驟 | 程式檢查 | 失敗提醒 |
|---|---|---|
| 啟動 | PVE 位址、Token、CA、node、owner、CSI socket 設定 | 指出缺少或不合法的設定 |
| `validate_request` | 名稱、容量、storage、支援的磁碟需求 | `InvalidArgument`／`OutOfRange`，指出不合法的欄位 |
| `check_storage_permissions` | Token 對指定儲存池的有效權限 | `PermissionDenied`，指出缺少的權限與路徑 |
| `check_owner_permissions` | Token 對指定 owner 的 `VM.Config.Disk` | `PermissionDenied`，例如 `/vms/7777` 缺 `VM.Config.Disk` |
| `check_storage_status` | 儲存池存在、啟用、可用，且為 LVM-thin／images | 不存在、未啟用、不可用或格式不支援，各自說明 |
| `lookup_volume` | 同名磁碟是否存在，容量與歸屬是否符合 | 符合就回傳；不符合回 `AlreadyExists`；查詢失敗就停止 |
| `check_allocation_permission` | 新建才要求 `Datastore.AllocateSpace` | `PermissionDenied`，指出 `/storage/<storage>` |
| `check_capacity` | 新建才檢查剩餘容量 | `ResourceExhausted`：空間不足 |
| `allocate_volume` | 只送一次建立要求 | 明確被拒絕時回報認證／權限等原因 |
| `confirm_volume` | 查回磁碟並核對識別、容量、歸屬 | 無法確認則回 `Unavailable`：磁碟可能已存在，保持原名稱與設定重試 |

第一個 PVE 請求也會驗證連線、憑證與 Token；失敗訊息會提示檢查網路、`PVE_ENDPOINT`、`PVE_CA_FILE` 或 Token。錯誤不包含 Token 或 PVE 原始回應內容。

權限檢查使用 Token 的實際有效權限，不看角色名稱。一般需要儲存池的 `Datastore.Audit` 加上 owner 的 `VM.Config.Disk`；已具 `Datastore.Allocate` 的帳號依 PVE 規則可略過 owner 檢查，但仍須通過清單 API 的權限檢查。查不到磁碟時會再查權限，避免把權限過濾當成不存在。權限與容量仍可能在檢查後改變，因此建碟後仍須查回確認。

依據：[PVE 磁碟存取權限](https://github.com/proxmox/pve-storage/blob/master/src/PVE/Storage.pm)、[PVE 建碟與清單 API](https://github.com/proxmox/pve-storage/blob/master/src/PVE/API2/Storage/Content.pm)。

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
