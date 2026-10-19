# pve-csi

讓 Kubernetes 的 PVC 在 PVE LVM-thin 建立磁碟，接到執行 Pod 的 VM，再由 VM 內的 CSI Node 格式化／掛載。Pod 離開時卸載、拔碟；刪除 PVC 時依 `reclaimPolicy: Delete` 回收磁碟。

PVE 權限、新增 node 的操作見 [PVE-SETUP.md](PVE-SETUP.md)。

先前 `dev-013` 實機結果見 [互動驗收報告](reports/volume-lifecycle.html)，驗收資源已移除。後續 review 修正的本機驗證不代表已重新完成實機驗收。

## 支援範圍

- 單一 controller、單一 PVE 主機；Linux node 都必須是該 PVE 主機上的 VM。
- LVM-thin／raw、ext4、`ReadWriteOnce`，容量至少 1 GiB、整 GiB 配置。
- Create／Delete／Validate、Controller Publish／Unpublish、Node Stage／Publish／Unpublish／Unstage。
- 不提供擴容、快照、複製、raw block 或跨 PVE 主機遷移。
- Kubernetes VM 需使用 VirtIO SCSI 並啟用 disk hotplug。預留 owner VMID 不可建立成 VM／容器。
- Node 需 Linux 5.12 以上，支援 `open_tree`、`mount_setattr`、`move_mount`；唯讀掛載準備好才會接到 Pod，不支援時會明確報錯。

## Controller 與 Node

```text
PVC → external-provisioner → Controller → PVE 在 storage 建立磁碟
Pod 選定 node → external-attacher → Controller → PVE 把磁碟接到對應 VM
VM 裡的 kubelet → CSI Node → 辨識磁碟、首次格式化 ext4、掛載給 Pod
刪除 Pod → kubelet 要求卸載 → Controller 要求 PVE 拔碟
刪除 PVC → Controller 確認無引用 → PVE 刪除磁碟
```

Node 用 VM 的 SMBIOS UUID 回報身分，Controller 查 PVE 對應 VMID；不寫死 VM100／101。磁碟保留預留 owner 的名稱，不隨 Pod 換 VM 而改名。

DaemonSet 自動部署到 Linux node，包括新增的節點，不用額外加 CSI opt-in 標籤。kubelet root 目前固定 `/var/lib/kubelet`，其他路徑不支援。混合非 PVE 節點的叢集不適用這份部署設定。

## DDD 分工

| 位置 | 責任 |
| --- | --- |
| `internal/domain` | 磁碟識別、容量、VM UUID，以及允許接碟／拔碟／刪碟的規則 |
| `internal/volume` | 安排檢查、讀取狀態、執行操作與確認結果；定義外部介面 |
| `internal/pve` | PVE API、權限、配置、背景工作與錯誤轉換 |
| `internal/linux` | 裝置、檔案系統、掛載表與 Linux 指令 |
| `internal/csi` | CSI 請求驗證、gRPC 回覆與錯誤碼 |
| `cmd/pve-csi` | 啟動與組裝 |

`domain` 不依賴外部套件；`volume` 不依賴 HTTP、PVE 權限名稱或 Linux 指令。後端錯誤在介接層轉成不可變的分類與安全提示，CSI 再轉成 gRPC 狀態。HTTPError 只保留 HTTP 事實。

建碟一律把原始名稱交給 `domain.NewRequest`，由 domain 產生穩定識別，呼叫端不用先雜湊。`VirtualMachine` 表示有 UUID 的 VM 候選；`ReferenceSource` 另外記錄 VM、模板或容器的磁碟引用，不能直接當成接拔碟目標。PVE 寫入前仍重新讀取配置並確認插槽與磁碟身分。

預留 owner VMID 由 `volume.Service.CheckReservedOwner` 統一檢查，啟動及後續建碟／接碟／刪碟流程共用；每次都重新查詢 PVE。

## 啟動與操作順序

Controller：**設定格式 → 實際連線與 Token 驗證 → 固定權限 → PVE 主機確認 → owner 編號未被占用 → 開放 CSI 服務**。

啟動必須具備 `/nodes/<PVE_NODE>` 的 `Sys.Audit` 與 `/vms` 向下傳播的 `VM.Audit`。缺少就直接報錯退出，指出權限與路徑，不會先回報 Ready。啟動檢查最多 30 秒，只做讀取。

Node：**設定與自己的 UUID → 掛載工具存在 → kubelet 目錄與掛載權限／核心功能 → 開放 CSI 服務**。檢查只建立尚未公開的掛載副本，確認唯讀功能後立即釋放；不格式化、不接碟、不改既有掛載。

儲存池由 StorageClass 指定，接碟的目標 VM 由 Kubernetes 後續要求帶入，啟動時還不知道。這兩類權限（包含讀取 owner 磁碟所需權限）在知道目標後檢查，不要求能操作所有儲存池或所有 VM，也不新增重複的環境變數。已有磁碟／已完成接拔的重試先確認現況，不要求額外的寫入權限。

啟動通過不代表永久授權：每次操作仍會重新檢查所需權限與狀態，全部通過才進行變更。接碟先驗證 node ID，再讀 PVE；掛載先驗證磁碟與掛載狀態，再建立目錄。

## 資料保護與失敗處理

| 檢查 | 不符合時 |
| --- | --- |
| 儲存池存在、啟用、格式、權限與容量 | 明確回覆權限不足、設定錯誤或容量不足 |
| 預留 owner VMID 未被 VM／容器使用 | 建碟、接碟、刪碟前統一檢查；被占用即拒絕 |
| UUID 唯一、目標 VM 有磁碟修改權限 | 拒絕接碟，指出檢查步驟 |
| 磁碟已在其他 VM、pending、snapshot 或 unused 設定中 | 拒絕重複接碟／刪碟 |
| VM 有待套用設定或配置工作尚未結束 | 暫停變更，要求處理設定或重試 |
| 能完整查看 PVE 主機背景工作 | 缺 `/nodes/<PVE_NODE>` 的 `Sys.Audit` 就拒絕以清單判斷閒置，不把權限過濾當成沒工作 |
| 磁碟專屬序號、裝置唯一、無分割區／其他使用者 | 不符就拒絕格式化 |
| 檢查明確回傳無磁碟簽章 | 才允許首次建立 ext4；缺資料或格式錯誤也拒絕 |
| 已有 ext4 | UUID 必須符合該磁碟，直接重用；不重新格式化 |
| 掛載路徑、實際裝置與掛載表一致 | 不符就拒絕掛載／卸載，不碰其他磁碟 |
| Stage 還有其他掛載引用 | 拒絕 Unstage，等 Pod 卸載 |
| 同一路徑已掛載但唯讀設定不同 | 回 `AlreadyExists`，先卸載再變更；不修改既有掛載 |
| 新的唯讀掛載設定失敗或程序突然死亡 | 掛載先在未公開狀態設好唯讀，再一次接到 Pod；公開前死亡由核心回收，公開後重試驗證既有掛載 |
| PVE 寫入後連線中斷或工作狀態未知 | 回結果未知，以相同識別重試；不猜成功、不自動回滾刪除 |

接碟選空的 SCSI 槽，不改 `scsi0`。PVE 配置寫入使用 digest 防止覆蓋其他變更；拔碟不送 `force`、不刪 `unusedN`。重啟後重新讀 PVE 與 Linux 掛載表，不靠本地操作紀錄。

單程序的鎖不能協調另一套 CSI 或管理員同時操作。此 driver 管理的磁碟不可手動接拔、改序號或建立快照。VM 停機且裝置不可見等異常情境可能需要人工確認，不提供強制拔碟／強制卸載。

## 設定

| 設定 | 位置／用途 |
| --- | --- |
| `CSI_DRIVER_NAME` | `deploy/config.env`；driver 身分與 topology 前綴 |
| `PVE_ENDPOINT` | 同上；完整 HTTPS URL，名稱／IP 必須符合憑證 |
| `PVE_NODE` | 同上；PVE API 的主機名稱 |
| `PVE_OWNER_VMID` | 同上；預留磁碟歸屬編號 |
| `PVE_TOKEN_ID`、`PVE_TOKEN_SECRET` | `data/dev/.env`；部署為 Secret |
| `PVE_CA_FILE` | 預設不設定；啟用私人 CA 掛載時設為 `/etc/pve/ca.crt`；本機執行可指定 CA 路徑 |
| 儲存池 | `deploy/storageclass.yaml` 的 `parameters.storage` |
| 程式／映像版本 | CI 提供 Git tag 或 commit；build 注入版本，部署 overlay 指定相同映像標籤 |

預設使用系統信任的 CA，不掛載額外憑證。PVE 使用私人 CA 時，才準備 `data/dev/pve-ca.pem` 並啟用下方的私人 CA 設定；兩種方式都會驗證憑證與網址名稱。

Node 只讀 driver 名稱與 PVE node，不攜帶 PVE 或 Kubernetes API 憑證。Node 為掛載主機檔案系統使用 privileged、主機 `/dev` 與 `/var/lib/kubelet` 雙向掛載傳播；Controller 以非 root 執行。

## home-lab 部署

先確認 [PVE 權限](PVE-SETUP.md)。以下使用 Kubernetes context `home-lab`，不要省略 context。

版本不寫入原始碼。CI 用 `--build-arg VERSION=<tag-or-commit>`；直接 Go build 則用 `-ldflags "-X main.version=<tag-or-commit>"`。以下以 Git 產生本機版本，部署 overlay 放在已忽略的 `data/dev`。

```sh
CSI_BUILD_VERSION="$(rtk proxy git describe --tags --always --dirty)"
rtk proxy docker --context=orbstack build --platform linux/amd64 --build-arg VERSION="$CSI_BUILD_VERSION" -t "pve-csi:$CSI_BUILD_VERSION" .
rtk proxy docker --context=orbstack save -o /tmp/pve-csi-image.tar "pve-csi:$CSI_BUILD_VERSION"
# 把 tar 傳到每台 node，再在各 VM 執行：
# sudo k3s ctr images import /tmp/pve-csi-image.tar
# rm /tmp/pve-csi-image.tar
rtk proxy mkdir -p data/dev/deploy
rtk proxy tee data/dev/deploy/kustomization.yaml >/dev/null <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: [../../../deploy]
images:
  - name: pve-csi
    newTag: "$CSI_BUILD_VERSION"
EOF
```

**只有 PVE 使用私人 CA 時**，才在剛產生的 `data/dev/deploy/kustomization.yaml` 加上這段；它會同時加入 CA 掛載與 `PVE_CA_FILE`：

```yaml
components:
  - ../../../deploy/private-ca
```

套用部署並建立 Token Secret：

```sh
rtk proxy kubectl --context=home-lab apply -k data/dev/deploy
rtk proxy kubectl --context=home-lab -n pve-csi create secret generic pve-csi-pve --from-env-file=data/dev/.env
```

**有啟用私人 CA 才執行**，未提供時 Controller 會等待此 ConfigMap：

```sh
rtk proxy kubectl --context=home-lab -n pve-csi create configmap pve-csi-ca --from-file=ca.crt=data/dev/pve-ca.pem
```

最後確認啟動結果：

```sh
rtk proxy kubectl --context=home-lab -n pve-csi rollout status deployment/pve-csi-controller --timeout=120s
rtk proxy kubectl --context=home-lab -n pve-csi rollout status daemonset/pve-csi-node --timeout=120s
```

這份本機驗收部署使用 `imagePullPolicy: Never`；新增 node 時也需匯入同一映像。改用可存取的 registry 與 `IfNotPresent` 後，新增 node 才能自動下載，不需要逐台匯入。

Node 就緒還需確認 `CSINode` 有 driver 的 UUID、node 上有 `<driver>/pve-node` topology。PVC Bound 只代表磁碟已建立；Pod 能寫入、重建後讀回才代表掛載完整。

## 排錯與移除

健康檢查會透過 Unix socket 呼叫 CSI `Probe`，不只確認檔案存在。啟動、就緒與存活檢查都有逾時。啟動前必須通過上述檢查；啟動後的 Probe 不再反覆查 PVE，因此 PVE 暫時斷線不會觸發存活檢查重啟。Probe 確認 CSI 初始化完成且能回應，不代表當下 PVE 或磁碟可用。

CSI 操作輸出 JSON 紀錄：`request_id`、`method`、有效的 `volume_id`／`node_id`、`status`、`duration_ms`，失敗時加上安全的 `step` 與 `detail`。建碟前尚無磁碟 ID 時用 `volume_name_hash` 對照重試；Linux 指令紀錄名稱、耗時與退出碼。不記錄完整請求、Token、指令參數或原始輸出。

```sh
rtk proxy kubectl --context=home-lab -n pve-csi get pods,pvc -o wide
rtk proxy kubectl --context=home-lab get pv,volumeattachments,csinodes
rtk proxy kubectl --context=home-lab -n pve-csi get events --sort-by=.lastTimestamp
rtk proxy kubectl --context=home-lab -n pve-csi logs deployment/pve-csi-controller -c provisioner
rtk proxy kubectl --context=home-lab -n pve-csi logs deployment/pve-csi-controller -c attacher
rtk proxy kubectl --context=home-lab -n pve-csi logs deployment/pve-csi-controller -c csi
rtk proxy kubectl --context=home-lab -n pve-csi logs -l app=pve-csi-node -c csi --prefix
```

清除順序：**刪除使用磁碟的 Pod → 等待卸載／拔碟 → 刪 PVC → 確認 PV、VolumeAttachment 與 PVE 磁碟都消失 → 才移除 driver**。不要先刪 controller 或強拆 finalizer。

完全沒有此 driver 的磁碟在使用後，才能執行 `rtk proxy kubectl --context=home-lab delete -k deploy`，並移除 `pve-csi` namespace 的 Secret／CA。保留其他 driver 的 CSINode 項目；僅清除本 driver 遺留的 topology 標籤與空 socket 目錄。

## 本機驗證

```sh
rtk proxy env GOTOOLCHAIN=go1.27.2 go test -race ./...
rtk proxy env GOTOOLCHAIN=go1.27.2 go vet ./...
```

一般測試使用本機模擬 PVE 與保留狀態的模擬掛載環境，不需要家中憑證。涵蓋回覆遺失、重啟重試、同時操作與健康檢查逾時。

`internal/linux/bind_integration_test.go` 需明確指定 `-tags=integration`，在有 `CAP_SYS_ADMIN` 的隔離 Linux 容器執行。測試只建立私人 mount namespace 與 tmpfs，強制殺掉子程序驗證唯讀掛載中斷行為，不使用實體磁碟。這不取代完整 PVE／Kubernetes 實機驗收。

依據：[CSI 規範](https://github.com/container-storage-interface/spec/blob/v1.13.0/spec.md)、[PVE VM 配置 API](https://github.com/proxmox/qemu-server/blob/master/src/PVE/API2/Qemu.pm)、[PVE 拔碟行為](https://github.com/proxmox/qemu-server/blob/master/src/PVE/QemuServer.pm)、[external-attacher](https://github.com/kubernetes-csi/external-attacher)。
