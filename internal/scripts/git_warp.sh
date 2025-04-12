## git 的命令包装
function my_git() {
    if [[ "$1" == "push" ]]; then
        local push_params="${@:2}"

        # 检查git push命令，如果有多个参数，则检查是否存在 b1:b2 的结构
        # 如果存在，则执行git push 后会执行 git branch 将本地分支与远程分支关联
        if [[ $# -gt 1 ]]; then
            # echo "执行 git push，参数为：$push_params"

            local ct=$(echo "$push_params" | grep -oE "([^ ]+):([^ ]+)")
            if [ ct ]; then
                # 设置 IFS 为冒号，并读取到两个变量中
                IFS=':' read -r source_branch target_branch <<< "$ct"

                # echo "source Branch：$source_branch"
                # echo "target Branch：$target_branch"
                # echo "branch --set-upstream-to=origin/$target_branch $source_branch"

                command git "$@"; # 执行push
                command git branch "--set-upstream-to=origin/$target_branch" "$source_branch" # 关联分支  #  git branch --set-upstream-to=origin/<branch> tt
            else
                echo "未找到两个分支"
                command git "$@"
            fi

        else
            echo "git push 命令没有指定参数。"
            command git "$@"
        fi

    else
        # 直接用原生git命令执行其他非push命令
        command git "$@"
    fi
}

# 创建别名
alias git='my_git'
