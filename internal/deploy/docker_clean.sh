#!/bin/bash

set -e

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
PURPLE='\033[0;35m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

# 日志函数
log_info() {
    echo -e "${BLUE}ℹ️  $1${NC}"
}

log_success() {
    echo -e "${GREEN}✅ $1${NC}"
}

log_warning() {
    echo -e "${YELLOW}⚠️  $1${NC}"
}

log_error() {
    echo -e "${RED}❌ $1${NC}"
}

log_header() {
    echo -e "${PURPLE}🐳 $1${NC}"
}

# 显示帮助信息
show_help() {
    echo -e "${CYAN}Docker 清理脚本使用说明${NC}"
    echo ""
    echo "用法: $0 [选项]"
    echo ""
    echo "选项:"
    echo "  -h, --help          显示此帮助信息"
    echo "  -l, --light         轻度清理（悬空镜像、停止的容器）"
    echo "  -m, --medium        中度清理（未使用的资源）"
    echo "  -d, --deep          深度清理（包括未使用的镜像）"
    echo "  -f, --full          完全清理（包括数据卷，危险！）"
    echo "  -c, --compose       清理 Docker Compose 资源"
    echo "  -s, --status        显示 Docker 空间使用情况"
    echo "  -i, --interactive   交互式清理模式"
    echo "  --dry-run          预览清理操作（不实际执行）"
    echo ""
    echo "示例:"
    echo "  $0 -l               # 轻度清理"
    echo "  $0 -m               # 中度清理"
    echo "  $0 -d               # 深度清理"
    echo "  $0 -c               # 清理 Compose 资源"
    echo "  $0 -i               # 交互式模式"
    echo ""
}

# 显示 Docker 空间使用情况
show_docker_status() {
    log_header "Docker 空间使用情况"
    docker system df
    echo ""

    log_info "详细信息:"
    docker system df -v
    echo ""
}

# 确认操作
confirm_action() {
    local message="$1"
    local default="${2:-n}"

    if [[ "$DRY_RUN" == "true" ]]; then
        log_warning "[预览模式] $message"
        return 0
    fi

    echo -e "${YELLOW}$message (y/N): ${NC}"
    read -r response
    response=${response:-$default}

    if [[ "$response" =~ ^[Yy]$ ]]; then
        return 0
    else
        return 1
    fi
}

# 执行命令（支持预览模式）
execute_command() {
    local cmd="$1"
    local description="$2"

    if [[ "$DRY_RUN" == "true" ]]; then
        log_warning "[预览] $description"
        log_info "将执行: $cmd"
        return 0
    fi

    log_info "$description"
    if eval "$cmd"; then
        log_success "完成: $description"
    else
        log_error "失败: $description"
        return 1
    fi
}

# 轻度清理
light_cleanup() {
    log_header "开始轻度清理"

    execute_command "docker container prune -f" "清理已停止的容器"
    execute_command "docker image prune -f" "清理悬空镜像"
    execute_command "docker network prune -f" "清理未使用的网络"

    log_success "轻度清理完成"
}

# 中度清理
medium_cleanup() {
    log_header "开始中度清理"

    execute_command "docker system prune -f" "清理未使用的容器、网络、镜像和构建缓存"

    log_success "中度清理完成"
}

# 深度清理
deep_cleanup() {
    log_header "开始深度清理"

    if confirm_action "这将删除所有未使用的镜像，确定继续吗？"; then
        execute_command "docker system prune -a -f" "清理所有未使用的资源（包括镜像）"
        execute_command "docker builder prune -f" "清理构建缓存"
    else
        log_warning "深度清理已取消"
        return 1
    fi

    log_success "深度清理完成"
}

# 完全清理
full_cleanup() {
    log_header "开始完全清理"

    log_warning "⚠️  完全清理将删除所有未使用的资源，包括数据卷！"
    log_warning "⚠️  这可能会导致数据丢失！"

    if confirm_action "确定要执行完全清理吗？这是一个危险操作！"; then
        if confirm_action "最后确认：真的要删除所有未使用的资源包括数据卷吗？"; then
            execute_command "docker system prune -a -f --volumes" "清理所有未使用的资源（包括数据卷）"
        else
            log_warning "完全清理已取消"
            return 1
        fi
    else
        log_warning "完全清理已取消"
        return 1
    fi

    log_success "完全清理完成"
}

# Docker Compose 清理
compose_cleanup() {
    log_header "开始 Docker Compose 清理"

    # 查找当前目录及子目录中的 docker-compose.yml 文件
    local compose_files
    compose_files=$(find . -name "docker-compose.yml" -o -name "docker-compose.yaml" 2>/dev/null)

    if [[ -z "$compose_files" ]]; then
        log_warning "未找到 docker-compose.yml 文件"
        return 1
    fi

    log_info "找到以下 Compose 文件:"
    echo "$compose_files"
    echo ""

    if confirm_action "是否清理所有找到的 Compose 项目？"; then
        while IFS= read -r compose_file; do
            local dir
            dir=$(dirname "$compose_file")
            log_info "清理项目: $dir"

            execute_command "cd '$dir' && docker-compose down -v --rmi local --remove-orphans" "清理 $dir 中的 Compose 资源"
        done <<< "$compose_files"
    else
        log_warning "Compose 清理已取消"
        return 1
    fi

    log_success "Docker Compose 清理完成"
}

# 交互式清理
interactive_cleanup() {
    log_header "交互式清理模式"

    while true; do
        echo ""
        echo -e "${CYAN}请选择清理选项:${NC}"
        echo "1) 显示空间使用情况"
        echo "2) 轻度清理（悬空镜像、停止的容器）"
        echo "3) 中度清理（未使用的资源）"
        echo "4) 深度清理（包括未使用的镜像）"
        echo "5) 完全清理（包括数据卷，危险！）"
        echo "6) Docker Compose 清理"
        echo "7) 自定义清理"
        echo "0) 退出"
        echo ""
        echo -n "请输入选项 (0-7): "

        read -r choice

        case $choice in
            1)
                show_docker_status
                ;;
            2)
                light_cleanup
                ;;
            3)
                medium_cleanup
                ;;
            4)
                deep_cleanup
                ;;
            5)
                full_cleanup
                ;;
            6)
                compose_cleanup
                ;;
            7)
                custom_cleanup
                ;;
            0)
                log_info "退出交互式模式"
                break
                ;;
            *)
                log_error "无效选项，请重新选择"
                ;;
        esac
    done
}

# 自定义清理
custom_cleanup() {
    log_header "自定义清理选项"

    echo -e "${CYAN}选择要清理的资源类型:${NC}"
    echo "1) 容器 (containers)"
    echo "2) 镜像 (images)"
    echo "3) 网络 (networks)"
    echo "4) 数据卷 (volumes)"
    echo "5) 构建缓存 (build cache)"
    echo "6) 全部"
    echo ""
    echo -n "请输入选项 (1-6): "

    read -r choice

    case $choice in
        1)
            if confirm_action "清理所有已停止的容器？"; then
                execute_command "docker container prune -f" "清理容器"
            fi
            ;;
        2)
            echo "镜像清理选项:"
            echo "a) 仅悬空镜像"
            echo "b) 所有未使用的镜像"
            echo -n "请选择 (a/b): "
            read -r img_choice

            case $img_choice in
                a)
                    execute_command "docker image prune -f" "清理悬空镜像"
                    ;;
                b)
                    if confirm_action "清理所有未使用的镜像？"; then
                        execute_command "docker image prune -a -f" "清理所有未使用的镜像"
                    fi
                    ;;
                *)
                    log_error "无效选项"
                    ;;
            esac
            ;;
        3)
            execute_command "docker network prune -f" "清理网络"
            ;;
        4)
            if confirm_action "清理未使用的数据卷？这可能导致数据丢失！"; then
                execute_command "docker volume prune -f" "清理数据卷"
            fi
            ;;
        5)
            execute_command "docker builder prune -f" "清理构建缓存"
            ;;
        6)
            if confirm_action "清理所有类型的资源？"; then
                medium_cleanup
            fi
            ;;
        *)
            log_error "无效选项"
            ;;
    esac
}

# 清理前后对比
cleanup_with_comparison() {
    log_header "清理前后对比"

    log_info "清理前的空间使用情况:"
    docker system df
    echo ""

    # 执行清理操作
    case "$1" in
        "light")
            light_cleanup
            ;;
        "medium")
            medium_cleanup
            ;;
        "deep")
            deep_cleanup
            ;;
        "full")
            full_cleanup
            ;;
        *)
            log_error "未知的清理类型: $1"
            return 1
            ;;
    esac

    echo ""
    log_info "清理后的空间使用情况:"
    docker system df
    echo ""

    log_success "清理对比完成"
}

# 主函数
main() {
    local LIGHT=false
    local MEDIUM=false
    local DEEP=false
    local FULL=false
    local COMPOSE=false
    local STATUS=false
    local INTERACTIVE=false
    DRY_RUN=false

    # 解析命令行参数
    while [[ $# -gt 0 ]]; do
        case $1 in
            -h|--help)
                show_help
                exit 0
                ;;
            -l|--light)
                LIGHT=true
                shift
                ;;
            -m|--medium)
                MEDIUM=true
                shift
                ;;
            -d|--deep)
                DEEP=true
                shift
                ;;
            -f|--full)
                FULL=true
                shift
                ;;
            -c|--compose)
                COMPOSE=true
                shift
                ;;
            -s|--status)
                STATUS=true
                shift
                ;;
            -i|--interactive)
                INTERACTIVE=true
                shift
                ;;
            --dry-run)
                DRY_RUN=true
                shift
                ;;
            *)
                log_error "未知参数: $1"
                show_help
                exit 1
                ;;
        esac
    done

    # 检查 Docker 是否运行
    if ! docker info >/dev/null 2>&1; then
        log_error "Docker 未运行或无法连接"
        exit 1
    fi

    log_header "Docker 清理脚本启动"

    if [[ "$DRY_RUN" == "true" ]]; then
        log_warning "运行在预览模式，不会实际执行清理操作"
    fi

    # 执行相应的操作
    if [[ "$STATUS" == "true" ]]; then
        show_docker_status
    elif [[ "$INTERACTIVE" == "true" ]]; then
        interactive_cleanup
    elif [[ "$LIGHT" == "true" ]]; then
        cleanup_with_comparison "light"
    elif [[ "$MEDIUM" == "true" ]]; then
        cleanup_with_comparison "medium"
    elif [[ "$DEEP" == "true" ]]; then
        cleanup_with_comparison "deep"
    elif [[ "$FULL" == "true" ]]; then
        cleanup_with_comparison "full"
    elif [[ "$COMPOSE" == "true" ]]; then
        compose_cleanup
    else
        # 默认显示帮助信息
        show_help
        echo ""
        log_info "提示: 使用 -i 或 --interactive 进入交互式模式"
    fi

    log_success "脚本执行完成"
}

# 脚本入口
main "$@"
