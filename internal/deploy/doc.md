
## 概述

Docker 在长期使用过程中会积累大量的镜像、容器、网络和数据卷，占用大量磁盘空间。本教程将帮助你有效地清理这些资源，释放磁盘空间。

## Docker 空间清理

### 1. 查看 Docker 空间使用情况

```bash
# 查看 Docker 系统整体空间使用情况
docker system df

# 查看详细信息
docker system df -v
```

### 2. 清理未使用的容器

```bash
# 删除所有已停止的容器
docker container prune

# 强制删除所有容器（包括运行中的）
docker rm -f $(docker ps -aq)

# 删除特定状态的容器
docker rm $(docker ps -a -f status=exited -q)
```

### 3. 清理未使用的镜像

```bash
# 删除悬空镜像（dangling images）
docker image prune

# 删除所有未使用的镜像
docker image prune -a

# 删除特定镜像
docker rmi <image_id>

# 批量删除镜像
docker rmi $(docker images -q)
```

### 4. 清理未使用的网络

```bash
# 删除未使用的网络
docker network prune

# 查看所有网络
docker network ls

# 删除特定网络
docker network rm <network_name>
```

### 5. 清理未使用的数据卷

```bash
# 删除未使用的数据卷
docker volume prune

# 查看所有数据卷
docker volume ls

# 删除特定数据卷
docker volume rm <volume_name>

# 删除所有数据卷（危险操作！）
docker volume rm $(docker volume ls -q)
```

### 6. 一键清理所有未使用资源

```bash
# 清理所有未使用的容器、网络、镜像和构建缓存
docker system prune

# 包括未使用的镜像（更彻底）
docker system prune -a

# 包括数据卷（最彻底，但要小心！）
docker system prune -a --volumes
```

## Docker Compose 空间清理

### 1. 停止并删除 Compose 服务

```bash
# 停止并删除容器、网络
docker-compose down

# 同时删除数据卷
docker-compose down -v

# 同时删除镜像
docker-compose down --rmi all

# 删除所有资源（容器、网络、镜像、数据卷）
docker-compose down -v --rmi all --remove-orphans
```

### 2. 清理特定项目的资源

```bash
# 进入项目目录
cd /path/to/your/project

# 停止服务
docker-compose stop

# 删除容器
docker-compose rm -f

# 删除未使用的网络
docker network prune

# 删除项目相关的镜像
docker-compose down --rmi local
```

### 3. 批量清理多个 Compose 项目

```bash
# 查找所有 docker-compose.yml 文件并清理
find . -name "docker-compose.yml" -execdir docker-compose down -v --rmi all \;
```

## 自动化清理脚本

### 创建清理脚本

创建一个名为 `docker-cleanup.sh` 的脚本：

```bash
#!/bin/bash

echo "🧹 开始 Docker 空间清理..."

echo "📊 清理前的空间使用情况："
docker system df

echo "🗑️  清理已停止的容器..."
docker container prune -f

echo "🖼️  清理悬空镜像..."
docker image prune -f

echo "🌐 清理未使用的网络..."
docker network prune -f

echo "💾 清理未使用的数据卷..."
docker volume prune -f

echo "🏗️  清理构建缓存..."
docker builder prune -f

echo "📊 清理后的空间使用情况："
docker system df

echo "✅ Docker 清理完成！"
```

### 使脚本可执行

```bash
chmod +x docker-cleanup.sh
```

### 定期自动清理（可选）

添加到 crontab 中实现定期清理：

```bash
# 编辑 crontab
crontab -e

# 添加以下行（每周日凌晨 2 点执行清理）
0 2 * * 0 /path/to/docker-cleanup.sh >> /var/log/docker-cleanup.log 2>&1
```

## 最佳实践

### 1. 定期清理策略

```bash
# 每日轻度清理（只清理悬空资源）
docker image prune -f
docker container prune -f

# 每周中度清理（清理未使用的资源）
docker system prune -f

# 每月深度清理（包括未使用的镜像）
docker system prune -a -f
```

### 2. 镜像管理最佳实践

```bash
# 使用多阶段构建减少镜像大小
# 在 Dockerfile 中使用 .dockerignore
# 定期更新基础镜像

# 清理特定时间之前的镜像
docker images --format "table {{.Repository}}\t{{.Tag}}\t{{.CreatedAt}}" | grep "weeks ago" | awk '{print $1":"$2}' | xargs docker rmi
```

### 3. 数据卷管理

```bash
# 备份重要数据卷
docker run --rm -v <volume_name>:/data -v $(pwd):/backup alpine tar czf /backup/backup.tar.gz /data

# 恢复数据卷
docker run --rm -v <volume_name>:/data -v $(pwd):/backup alpine tar xzf /backup/backup.tar.gz -C /
```

## 故障排除

### 1. 无法删除正在使用的资源

```bash
# 查看哪些容器在使用镜像
docker ps -a --filter ancestor=<image_name>

# 强制停止所有容器
docker kill $(docker ps -q)

# 然后再尝试删除
docker system prune -a -f
```

### 2. 权限问题

```bash
# 如果遇到权限问题，使用 sudo
sudo docker system prune -a -f

# 或者将用户添加到 docker 组
sudo usermod -aG docker $USER
```

### 3. 磁盘空间不足

```bash
# 紧急情况下的强制清理
docker system prune -a -f --volumes

# 清理 Docker 根目录（极端情况）
sudo systemctl stop docker
sudo rm -rf /var/lib/docker
sudo systemctl start docker
```

### 4. 检查具体占用空间的资源

```bash
# 查看最大的镜像
docker images --format "table {{.Repository}}\t{{.Tag}}\t{{.Size}}" | sort -k3 -h

# 查看容器大小
docker ps -s

# 查看数据卷大小
docker system df -v
```

## 监控和警报

### 创建磁盘空间监控脚本

```bash
#!/bin/bash
# disk-monitor.sh

THRESHOLD=80
USAGE=$(df /var/lib/docker | tail -1 | awk '{print $5}' | sed 's/%//')

if [ $USAGE -gt $THRESHOLD ]; then
    echo "⚠️  Docker 磁盘使用率超过 ${THRESHOLD}%，当前使用率：${USAGE}%"
    echo "建议执行清理操作："
    echo "docker system prune -a -f"
fi
```

## 总结

定期清理 Docker 资源是维护系统健康的重要步骤。建议：

1. **每日**：清理悬空镜像和已停止容器
2. **每周**：执行 `docker system prune`
3. **每月**：执行 `docker system prune -a`
4. **按需**：在磁盘空间不足时执行深度清理
