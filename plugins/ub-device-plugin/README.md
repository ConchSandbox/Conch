# ub-device-plugin

ub-device-plugin是面向灵衢UB硬件开发的设备插件，支持设备的分配和销毁，
动态发现等功能。以便在云原生部署场景下，容器能够方便的访问UB硬件。目前
主要支持以下设备：

- obmm设备：`/dev/obmm`, `/dev/obmm_shmdev*`

关于obmm的更多信息请参考： https://atomgit.com/openeuler/obmm

