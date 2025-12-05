# Conch

## 介绍

Conch是面向人工智能时代的沙箱引擎，Docker（集装箱）装载和分发货物，Conch（海螺）装载和分发智能体。
Conch围绕AI时代出现的AI推理、Agent应用等新业务，超节点、新总线、异构算力等新硬件构建更加高弹性、高性价比和高性能的容器底座。

## 软件架构

北向支持对接云原生平台（K8S）、AI原生平台和极简单机部署模式。

南向支持Rack级资源共享，构建容器镜像懒加载、原生快照镜像、共享内存文件系统等特性实现高弹性和高密部署。

## 安装使用

使用yum安装：
```shell
yum install conch-0.1.xx.rpm
```

构建快照镜像：
```shell
conch build Dockerfile -t agent-sandbox-template:v1
```

从快照启动沙箱：
```shell
conch restore agent-sandbox-template:v1
```


## 参与贡献

1.  Fork 本仓库
2.  新建特性分支
3.  提交代码
4.  发起 Pull Request


## 许可证

木兰宽松许可证， 第2版
