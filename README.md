# pve-csi

目前提供 CSI `CreateVolume`、`DeleteVolume`、`ValidateVolumeCapabilities`、Identity 與 Node 身分查詢服務。可透過 external-provisioner 讓 PVC 自動建碟、刪碟；支援 PVE LVM-thin／raw 磁碟。Node 可回報 VM UUID，PVE 查詢可將 UUID 對應到 VMID；尚未提供接到 VM 或 Pod 掛載。

## DDD 分層

| 目錄 | 責任 |
|---|---|
| `cmd/pve-csi` | 啟動、設定與組裝 |
| `internal/domain` | 磁碟、容量、識別、VM UUID 與儲存池規則 |
| `internal/volume` | 建碟、刪碟、查詢與節點辨識流程，以及外部查詢介面 |
| `internal/csi` | CSI gRPC 服務、請求驗證與錯誤轉換 |
| `internal/pve` | HTTPS client、PVE API 與儲存介面實作 |

`csi` 呼叫同一個 `volume.Service` 處理建碟、查詢與刪碟；Service 統一檢查 node／owner 範圍，再透過 `StorageGateway` 呼叫 PVE。`volume` 只依賴 `domain`，`pve` 實作儲存介面；`domain` 不依賴 CSI 或 PVE。

應用層決定檢查時機：既有磁碟不要求新增空間權限；新建前重新查寫入權限；刪除前由應用層確認 owner 未被 VM／CT 使用。PVE 權限名稱、HTTP 狀態與 API 回應解析只留在 `pve`，不透過 `PoolState` 或字串欄位傳進應用規則。

| 錯誤位置 | 責任 |
|---|---|
| `internal/domain` | 容量、身分、衝突等規則錯誤 |
| `internal/volume/errors.go` | 操作結果、失敗步驟與不可變的後端錯誤分類 |
| `internal/pve/errors.go` | HTTP／TLS／PVE 錯誤轉換；`ErrStorageNotFound` 保持獨立，不在 `HTTPError` 加狀態旗標 |
| `internal/csi/errors.go` | 轉成 gRPC 狀態與安全的錯誤提示，不回傳後端原始內容 |

儲存池由每筆 CSI 請求的 `parameters.storage` 指定；磁碟歸屬編號讀取 `PVE_OWNER_VMID`。

## Node 身分辨識

Node 元件讀 Linux 的 `/sys/class/dmi/id/product_uuid`，以正規化 UUID 回覆 `NodeGetInfo.node_id`，並使用既有 `<CSI_DRIVER_NAME>/pve-node` 回報單一 PVE 主機的 topology。UUID 不由環境變數或 VM 名稱推測。

```sh
# 在 PVE 裡的 Linux VM 執行，帳號必須能讀取 product_uuid。
CSI_DRIVER_NAME=pve-csi.local PVE_NODE=pve \
  ./pve-csi node --endpoint=unix:///tmp/pve-csi-node.sock
```

Node 模式只讀 `CSI_DRIVER_NAME`、`PVE_NODE`，不需要 PVE Token、endpoint、CA 或 owner。容器需把主機 UUID 檔案唯讀掛到相同路徑；目前 home-lab 檔案為 root 擁有、權限 0400，因此 Node 程序須以 root 讀取，不能直接沿用 Controller 的非 root 設定。讀不到或 UUID 無效時啟動失敗，不開啟 CSI socket。

`volume.NodeResolver` 每次透過 PVE 介接層查詢指定 `PVE_NODE` 的非範本 QEMU VM，使用 `config?current=1` 讀取生效的 `smbios1.uuid`。全部必要查詢成功且 UUID 唯一才回傳 VMID；不快取，也不做磁碟操作。

| 失敗步驟 | 回覆 |
|---|---|
| `validate_node_id` | UUID 格式錯誤：`InvalidArgument` |
| `check_vm_permissions` | 缺少 `/vms` 可傳遞的 `VM.Audit` 或候選 VM 的有效讀取權限：`PermissionDenied` |
| `list_vms`／`read_vm_identity` | 認證、權限、網路與無效回應沿用既有分類；中途失敗不回傳部分結果 |
| `resolve_node` | 查無相符 VM：`NotFound`；多台 VM 使用相同 UUID：`FailedPrecondition` |

此自動辨識要求 `/vms` 範圍內完整的 `VM.Audit`，不能有 VM／Pool 的 `NoAccess` 覆寫。PVE 會隱藏沒有權限的 VM，父層權限檢查不能證明不存在這類隱藏項目；查無相符僅指此次可見範圍。每個 VM 的 UUID 必須唯一。查詢不是鎖，後續接碟仍須重新核對身分、權限與實際配置。

目前完成 Node RPC、唯讀解析，以及 DaemonSet／registrar 部署。kubelet 會把 UUID 寫進 CSINode，並替節點加上 `<CSI_DRIVER_NAME>/pve-node` topology。尚未接入 `ControllerPublishVolume`；掛載 RPC 回 `Unimplemented`。`7777` 等 owner 是磁碟歸屬，解析出的 VMID 是未來接碟目標，兩者不能混用。

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
- **CSI socket**：用 `controller` 或 `node` 的 `--endpoint=unix:///path/to/csi.sock` 指定；省略時為 `/tmp/pve-csi.sock`。
- **映像版本**：`deploy/kustomization.yaml` 的 `images`。

HTTP 請求最多 10 秒、刪碟工作最多等待 30 秒、每半秒查進度，屬於內部執行策略。

## home-lab 部署與驗收

需要 Kubernetes 1.34 以上。`data/dev/.env` 只放上述兩個 Token 欄位，不加引號；CA 放 `data/dev/pve-ca.pem`。以下 Secret／CA 建立指令只需執行一次。

```sh
rtk proxy docker --context=orbstack build --platform linux/amd64 -t pve-csi:dev-011 .
rtk proxy docker --context=orbstack save -o /tmp/pve-csi-dev011.tar pve-csi:dev-011
rtk proxy scp -i ~/.ssh/id_ed25519_homelab /tmp/pve-csi-dev011.tar ubuntu@192.168.1.201:/tmp/pve-csi-dev011.tar
rtk proxy scp -i ~/.ssh/id_ed25519_homelab /tmp/pve-csi-dev011.tar ubuntu@192.168.1.202:/tmp/pve-csi-dev011.tar
rtk proxy ssh -i ~/.ssh/id_ed25519_homelab ubuntu@192.168.1.201 'sudo k3s ctr images import /tmp/pve-csi-dev011.tar && rm /tmp/pve-csi-dev011.tar'
rtk proxy ssh -i ~/.ssh/id_ed25519_homelab ubuntu@192.168.1.202 'sudo k3s ctr images import /tmp/pve-csi-dev011.tar && rm /tmp/pve-csi-dev011.tar'
rtk proxy rm /tmp/pve-csi-dev011.tar
rtk proxy kubectl --context=home-lab apply -k deploy
rtk proxy kubectl --context=home-lab -n pve-csi create secret generic pve-csi-pve --from-env-file=data/dev/.env
rtk proxy kubectl --context=home-lab -n pve-csi create configmap pve-csi-ca --from-file=ca.crt=data/dev/pve-ca.pem
rtk proxy kubectl --context=home-lab -n pve-csi rollout status deployment/pve-csi-controller --timeout=90s
```

使用 `apply -k deploy`，不要單獨 apply controller／StorageClass 模板。修改 config.env 後重新 apply，Pod 會自動更新；更換 Token／CA 或重建同名映像後，需 `rollout restart deployment/pve-csi-controller`。

### Node 註冊

DaemonSet 只部署到有 `<CSI_DRIVER_NAME>/node=true` 標籤的 Linux 節點。此標籤是管理員明確選擇部署目標；與 kubelet 自動寫入的 `/pve-node` topology 不同。以下使用目前 config.env 的 `pve-csi.local`，更換 driver 名稱時標籤前綴也須同步。

```sh
rtk proxy kubectl --context=home-lab label nodes k3s-master-01 k3s-worker-01 pve-csi.local/node=true
rtk proxy kubectl --context=home-lab -n pve-csi rollout status daemonset/pve-csi-node --timeout=90s
rtk proxy kubectl --context=home-lab get csinodes -o yaml
rtk proxy kubectl --context=home-lab get nodes -L pve-csi.local/pve-node
rtk proxy kubectl --context=home-lab -n pve-csi logs -l app=pve-csi-node -c registrar --prefix
```

驗收要同時看到：兩個 Node Pod 都是 `2/2 Ready`、registrar 回報 `plugin_registered:true`、CSINode 的 nodeID 與各 VM 的 `product_uuid` 一致、topology 值為設定的 PVE_NODE。單看 Pod Ready 不代表 kubelet 已完成註冊。

Node 只帶 CSI_DRIVER_NAME／PVE_NODE；不掛 PVE Token、CA 或 Kubernetes API token。讀取 UUID 需 root，但不使用 privileged、hostPID 或掛載能力。此階段不掛磁碟、不需 mount propagation。CSIDriver 保留 `attachRequired: true`；尚未實作接碟，不能用 Pod 寫檔驗收。

socket 目錄使用主機 `/var/lib/kubelet/plugins/<driver>`，註冊目錄為 `/var/lib/kubelet/plugins_registry`。這是目前 home-lab 的 kubelet 路徑，其他環境若更改 kubelet root，必須同步調整 node.yaml 的兩個 hostPath 與 registrar 參數。意外退出後會確認舊 socket 無人使用才回收；旁邊 `.lock` 是程序鎖，沒有操作紀錄，也不存磁碟狀態。

排錯：先看 `kubectl describe pod` 的事件，再看 `-c csi`（UUID／socket）與 `-c registrar`（註冊）的 logs。重建驗收可用 `rollout restart daemonset/pve-csi-node`，完成後重新核對 UUID。

只移除 Node 部署時：

```sh
rtk proxy kubectl --context=home-lab -n pve-csi delete daemonset pve-csi-node --wait=true
rtk proxy kubectl --context=home-lab label nodes k3s-master-01 k3s-worker-01 pve-csi.local/node-
rtk proxy kubectl --context=home-lab get csinodes -o yaml
```

等待 kubelet 移除該 driver 的 CSINode 項目；不要刪除整個 CSINode。 kubelet 可能保留 `/pve-node` topology 標籤；只有完全停用此 driver、沒有相關磁碟使用時，才明確移除本次新增的 topology 標籤。CSIDriver、Controller、StorageClass 若仍有 PVC/PV 使用，就保留。僅無磁碟使用且 Node 全部停止時，才可移除主機上此 driver 的 socket／lock 檔案。重新 `apply -k deploy` 會再建立 DaemonSet，但沒有 opt-in 標籤就不會派 Pod。

依據：[官方 registrar](https://github.com/kubernetes-csi/node-driver-registrar)、[CSIDriver 設定](https://kubernetes-csi.github.io/docs/csi-driver-object.html)。

### 建碟與回收

驗收（實際建立一顆 1 GiB 磁碟）：

```sh
rtk proxy kubectl --context=home-lab apply -f deploy/pvc.yaml
rtk proxy kubectl --context=home-lab -n pve-csi wait --for=jsonpath='{.status.phase}'=Bound pvc/pve-csi-smoke --timeout=90s
rtk proxy kubectl --context=home-lab -n pve-csi get pvc pve-csi-smoke -o wide
# 核對 PV 的 volumeHandle 與 PVE 磁碟後，才刪除驗收 PVC。
rtk proxy kubectl --context=home-lab delete -f deploy/pvc.yaml
```

刪除後確認 PV 與 PVE 磁碟都消失。失敗原因查看 `rtk proxy kubectl --context=home-lab -n pve-csi logs deployment/pve-csi-controller -c provisioner`。

目前只支援單一 controller／PVE node、LVM-thin／raw／ext4、單節點寫入、至少 1 GiB 且整 GiB 配置。`Bound` 只表示 PVC 與 PV 配對，尚未提供 VM 接碟或 Node 掛載。

已有 PV 時保留原 driver／node／owner 設定完成回收；舊 PV 不會自動遷移。更換 StorageClass 的 provisioner 或儲存池要建立新名稱的 StorageClass，不能原地修改。
