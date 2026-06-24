#!/usr/bin/env python3

# Copyright 2026 Flant JSC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
# 
#     http://www.apache.org/licenses/LICENSE-2.0
# 
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# run from repository root

"""
Скрипт для получения списка коммитов, которые не входят в последний тег.

Скрипт выполняет следующие действия:
1. Переключается на ветку main
2. Выполняет git fetch для получения обновлений
3. Находит последний тег
4. Отображает список коммитов после последнего тега
"""

import subprocess
import sys
import re
from typing import List, Optional, Tuple


def run_git_command(command: List[str], cwd: str = None) -> Tuple[bool, str]:
    """Выполняет git команду и возвращает результат."""
    try:
        result = subprocess.run(
            command,
            cwd=cwd,
            capture_output=True,
            text=True,
            check=True
        )
        return True, result.stdout.strip()
    except subprocess.CalledProcessError as e:
        return False, e.stderr.strip()


def get_current_branch() -> Optional[str]:
    """Получает текущую ветку."""
    success, output = run_git_command(["git", "branch", "--show-current"])
    if success and output:
        return output
    return None


def switch_to_main() -> bool:
    """Переключается на ветку main."""
    print("🔄 Переключение на ветку main...")
    
    # Проверяем, есть ли ветка main
    success, _ = run_git_command(["git", "show-ref", "--verify", "--quiet", "refs/heads/main"])
    if not success:
        # Пробуем origin/main
        success, _ = run_git_command(["git", "show-ref", "--verify", "--quiet", "refs/remotes/origin/main"])
        if not success:
            print("❌ Ветка main не найдена")
            return False
    
    # Переключаемся на main
    success, error = run_git_command(["git", "checkout", "main"])
    if not success:
        print(f"❌ Ошибка при переключении на main: {error}")
        return False
    
    print("✅ Успешно переключились на ветку main")
    return True


def fetch_updates() -> bool:
    """Выполняет git fetch для получения обновлений."""
    print("🔄 Получение обновлений (git fetch)...")
    
    success, error = run_git_command(["git", "fetch", "--all"])
    if not success:
        print(f"❌ Ошибка при выполнении git fetch: {error}")
        return False
    
    print("✅ Обновления получены успешно")
    return True


def get_latest_tag() -> Optional[str]:
    """Получает последний тег в репозитории."""
    print("🔍 Поиск последнего тега...")
    
    # Получаем все теги, отсортированные по дате
    success, output = run_git_command([
        "git", "tag", "--sort=-version:refname", "--merged"
    ])
    
    if not success or not output:
        print("⚠️  Теги не найдены")
        return None
    
    # Берем первый тег (самый новый)
    tags = output.split('\n')
    latest_tag = tags[0].strip()
    
    print(f"✅ Последний тег: {latest_tag}")
    return latest_tag


def get_commits_after_tag(tag: str) -> List[str]:
    """Получает список коммитов после указанного тега."""
    print(f"🔍 Поиск коммитов после тега {tag}...")
    
    # Получаем коммиты после тега
    success, output = run_git_command([
        "git", "log", f"{tag}..HEAD", "--oneline", "--no-merges"
    ])
    
    if not success:
        print(f"❌ Ошибка при получении коммитов: {output}")
        return []
    
    if not output:
        print("✅ Коммитов после последнего тега не найдено")
        return []
    
    commits = [line.strip() for line in output.split('\n') if line.strip()]
    print(f"✅ Найдено {len(commits)} коммитов после тега {tag}")
    
    return commits


def format_commit_info(commits: List[str]) -> str:
    """Форматирует информацию о коммитах для вывода."""
    if not commits:
        return "Коммитов после последнего тега не найдено."
    
    result = []
    result.append(f"\n📋 Список коммитов после последнего тега ({len(commits)} коммитов):")
    result.append("=" * 60)
    
    for i, commit in enumerate(commits, 1):
        # Разбираем коммит: hash и message
        parts = commit.split(' ', 1)
        if len(parts) == 2:
            commit_hash = parts[0]
            message = parts[1]
            result.append(f"{i:2d}. {commit_hash[:8]} - {message}")
        else:
            result.append(f"{i:2d}. {commit}")
    
    result.append("=" * 60)
    return "\n".join(result)


def get_commit_details(commits: List[str]) -> str:
    """Получает детальную информацию о коммитах."""
    if not commits:
        return ""
    
    result = []
    result.append("\n📊 Детальная информация о коммитах:")
    result.append("=" * 60)
    
    for i, commit in enumerate(commits[:10], 1):  # Показываем только первые 10
        commit_hash = commit.split(' ')[0]
        
        # Получаем детальную информацию о коммите
        success, details = run_git_command([
            "git", "show", "--stat", "--no-patch", commit_hash
        ])
        
        if success:
            lines = details.split('\n')
            commit_info = lines[0] if lines else commit
            result.append(f"\n{i}. {commit_info}")
            
            # Добавляем статистику изменений
            for line in lines[1:]:
                if line.strip() and ('file' in line.lower() or 'insertion' in line.lower() or 'deletion' in line.lower()):
                    result.append(f"   {line.strip()}")
    
    if len(commits) > 10:
        result.append(f"\n... и еще {len(commits) - 10} коммитов")
    
    result.append("=" * 60)
    return "\n".join(result)


def main():
    """Основная функция скрипта."""
    print("🚀 Скрипт для получения коммитов после последнего тега")
    print("=" * 60)
    
    # Проверяем, что мы в git репозитории
    success, _ = run_git_command(["git", "rev-parse", "--git-dir"])
    if not success:
        print("❌ Ошибка: текущая директория не является git репозиторием")
        return 1
    
    # Показываем текущую ветку
    current_branch = get_current_branch()
    if current_branch:
        print(f"📍 Текущая ветка: {current_branch}")
    
    # Переключаемся на main
    if not switch_to_main():
        return 1
    
    # Получаем обновления
    if not fetch_updates():
        return 1
    
    # Находим последний тег
    latest_tag = get_latest_tag()
    if not latest_tag:
        print("⚠️  Не удалось найти теги. Показываем все коммиты в ветке main.")
        # Если тегов нет, показываем все коммиты в main
        success, output = run_git_command([
            "git", "log", "--oneline", "--no-merges", "-20"
        ])
        if success and output:
            commits = [line.strip() for line in output.split('\n') if line.strip()]
            print(format_commit_info(commits))
        return 0
    
    # Получаем коммиты после тега
    commits = get_commits_after_tag(latest_tag)
    
    # Выводим результат
    print(format_commit_info(commits))
    
    # Показываем детальную информацию
    if commits:
        print(get_commit_details(commits))
    
    print("\n✅ Скрипт завершен успешно!")
    return 0


if __name__ == "__main__":
    exit(main())
