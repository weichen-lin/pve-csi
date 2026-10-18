# pve-csi

目前提供 CSI `CreateVolume`、`DeleteVolume`、`ValidateVolumeCapabilities` 與 Identity 服務。可透過 external-provisioner 讓 PVC 自動建碟、刪碟；支援 PVE LVM-thin／raw 磁碟。尚未提供接到 VM 或 Pod 掛載。

## DDD 分層

| 目錄 | 責任 |
|---|---|
| `cmd/pve-csi` | 啟動、設定與組裝 |
| `internal/domain` | 磁碟、容量、識別與儲存池規則 |
| `internal/volume` | 建碟、刪碟與查詢流程、儲存介面與權限檢查 |
| `internal/csi` | CSI gRPC 服務、請求驗證與錯誤轉換 |
| `internal/pve` | HTTPS client、PVE API 與儲存介面實作 |

`csi` 呼叫同一個 `volume.Service` 處理建碟、查詢與刪碟；Service 統一檢查 node／owner 範圍，再透過 `StorageGateway` 呼叫 PVE。`volume` 只依賴 `domain`，`pve` 實作儲存介面；`domain` 不依賴 CSI 或 PVE。

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

第一個 PVE 請求也會驗證連線、憑證與 Token；失敗訊息會提示檢查網路、`PVE_ENDPOINT`、憑證名稱／CA 或 Token。錯誤不包含 Token 或 PVE 原始回應內容。

權限檢查使用 Token 的實際有效權限，不看角色名稱。一般需要儲存池的 `Datastore.Audit` 加上 owner 的 `VM.Config.Disk`；已具 `Datastore.Allocate` 的帳號依 PVE 規則可略過 owner 檢查，但仍須通過清單 API 的權限檢查。查不到磁碟時會再查權限，避免把權限過濾當成不存在。權限與容量仍可能在檢查後改變，因此建碟後仍須查回確認。

依據：[PVE 磁碟存取權限](https://github.com/proxmox/pve-storage/blob/master/src/PVE/Storage.pm)、[PVE 建碟與清單 API](https://github.com/proxmox/pve-storage/blob/master/src/PVE/API2/Storage/Content.pm)。

## 刪碟檢查與錯誤

| 步驟 | 行為／失敗提醒 |
|---|---|
| `validate_delete_request`／`validate_delete_scope` | 只接受本服務的 v1 磁碟識別，node 與 owner 必須符合設定；格式錯誤或超出範圍就拒絕 |
| 儲存池與讀取權限 | 與建碟共用預檢；查詢失敗不會當成磁碟不存在 |
| `check_pending_deletion` | 同一 owner／storage 有可見的 PVE 刪碟工作仍在執行時，建碟與刪碟都回 `Aborted`，稍後重試 |
| `lookup_delete_volume`／`validate_delete_volume` | 確認磁碟不存在就成功；存在則核對識別、格式、容量及歸屬，不符就拒絕 |
| `check_deletion_permission` | 存在的磁碟要求 `/storage/<storage>` 的 `Datastore.Allocate`；不足回 `PermissionDenied` |
| `check_deletion_owner` | owner 必須是預留且未建立 VM／CT 的編號；已被使用回 `FailedPrecondition` |
| `delete_volume`／`wait_deletion_task` | 送出一次 DELETE，等待 PVE 背景工作完成（最多 30 秒，仍受呼叫端期限限制）；工作失敗提示查看 PVE Tasks |
| `confirm_delete` | 再查磁碟確實消失才成功；無法確認回 `Unavailable`，呼叫端期限到也可能收到 `DeadlineExceeded`，用相同 `volume_id` 重試 |

目前限單一 controller、專用 owner 下的未掛載磁碟；不可把這些磁碟手動接到其他 VM 或建立快照。owner 檢查不是跨 VM 的掛載／快照追蹤，也不是遠端鎖定。建刪操作在同一程序內共用磁碟鎖；重啟後透過 PVE 可見的背景工作避免立即重送，沒有本地操作紀錄檔。不同 controller 或管理員同時修改不在目前協調範圍。

`Datastore.Allocate` 是儲存池管理權限，比建碟的 `Datastore.AllocateSpace` 更大，應限定在專用儲存池。依據：[PVE 刪除 API](https://github.com/proxmox/pve-storage/blob/master/src/PVE/API2/Storage/Content.pm)、[PVE 工作狀態 API](https://github.com/proxmox/pve-manager/blob/master/PVE/API2/Tasks.pm)、[CSI DeleteVolume](https://github.com/container-storage-interface/spec/blob/v1.13.0/spec.md#deletevolume)。

## 環境設定

只有以下七個 runtime 環境變數；一般部署只需修改 `deploy/config.env` 的四項非機密設定。

| 變數 | 必要性／用途 | 設定位置 |
|---|---|---|
| `CSI_DRIVER_NAME` | 必填；CSI 身分，同步產生 StorageClass provisioner | `deploy/config.env` |
| `PVE_ENDPOINT` | 必填；PVE HTTPS 網址，IP／名稱須符合憑證 | 同上 |
| `PVE_NODE` | 必填；PVE 主機名稱 | 同上 |
| `PVE_OWNER_VMID` | 必填；預留、未建立 VM／CT 的磁碟歸屬編號 | 同上 |
| `PVE_TOKEN_ID` | 必填；API Token ID | `data/dev/.env` → Secret |
| `PVE_TOKEN_SECRET` | 必填；API Token 密鑰 | 同上 |
| `PVE_CA_FILE` | 選填；私有 CA 檔案路徑，省略則使用系統信任根 | 部署自動設定 `/etc/pve/ca.crt` |

目前直接連 `https://192.168.1.200:8006`，以網址內的 IP 驗證伺服器憑證。連線與憑證驗證共用同一個網址，不另設 server name，也不跳過憑證驗證。執行 CSI 的環境必須能連到這個位址。

其他設定各有唯一位置：

- **儲存池**：`deploy/storageclass.yaml` 的 `parameters.storage`，由每張 PVC 所選的 StorageClass 決定。
- **Topology key**：自動使用 `<CSI_DRIVER_NAME>/pve-node`，程式與部署同步產生。
- **CSI socket**：只用 `controller --endpoint=unix:///path/to/csi.sock` 指定；省略時為 `/tmp/pve-csi.sock`。
- **映像版本**：`deploy/kustomization.yaml` 的 `images`。

HTTP 請求最多 10 秒、刪碟工作最多等待 30 秒、每半秒查進度，屬於內部執行策略。

## OrbStack 部署與驗收

需要 Kubernetes 1.34 以上。`data/dev/.env` 只放上述兩個 Token 欄位，不加引號；CA 放 `data/dev/pve-ca.pem`。以下 Secret／CA 建立指令只需執行一次。

```sh
rtk proxy docker --context=orbstack build -t pve-csi:dev-009 .
rtk proxy kubectl --context=orbstack apply -k deploy
rtk proxy kubectl --context=orbstack -n pve-csi create secret generic pve-csi-pve --from-env-file=data/dev/.env
rtk proxy kubectl --context=orbstack -n pve-csi create configmap pve-csi-ca --from-file=ca.crt=data/dev/pve-ca.pem
rtk proxy kubectl --context=orbstack -n pve-csi rollout status deployment/pve-csi-controller --timeout=90s
```

使用 `apply -k deploy`，不要單獨 apply controller／StorageClass 模板。修改 config.env 後重新 apply，Pod 會自動更新；更換 Token／CA 或重建同名映像後，需 `rollout restart deployment/pve-csi-controller`。

驗收（實際建立一顆 1 GiB 磁碟）：

```sh
rtk proxy kubectl --context=orbstack apply -f deploy/pvc.yaml
rtk proxy kubectl --context=orbstack -n pve-csi wait --for=jsonpath='{.status.phase}'=Bound pvc/pve-csi-smoke --timeout=90s
rtk proxy kubectl --context=orbstack -n pve-csi get pvc pve-csi-smoke -o wide
# 核對 PV 的 volumeHandle 與 PVE 磁碟後，才刪除驗收 PVC。
rtk proxy kubectl --context=orbstack delete -f deploy/pvc.yaml
```

刪除後確認 PV 與 PVE 磁碟都消失。失敗原因查看 `rtk proxy kubectl --context=orbstack -n pve-csi logs deployment/pve-csi-controller -c provisioner`。

目前只支援單一 controller／PVE node、LVM-thin／raw／ext4、單節點寫入、至少 1 GiB 且整 GiB 配置。`Bound` 只表示 PVC 與 PV 配對，尚未提供 VM 接碟或 Node 掛載。

已有 PV 時保留原 driver／node／owner 設定完成回收；舊 PV 不會自動遷移。更換 StorageClass 的 provisioner 或儲存池要建立新名稱的 StorageClass，不能原地修改。
