#!/usr/bin/env python3
"""
从包含firecracker进程信息的文件中提取sandbox_id和数量
支持多文件输入，按文件分别统计

使用方法:
    python3 extract-sandbox-ids.py [选项] [file1] [file2] ...
    如果不提供文件，从stdin读取
    
    示例:
        python3 extract-sandbox-ids.py file1.txt file2.txt
        python3 extract-sandbox-ids.py --verbose file1.txt file2.txt
        ps aux | grep firecracker | python3 extract-sandbox-ids.py
"""

import sys
import re
import json
import os
import glob
import argparse
from collections import Counter
from pathlib import Path


def extract_sandbox_id(socket_path):
    """
    从socket路径中提取sandbox_id
    格式: /tmp/fc-{sandboxID}-{randomID}.sock
    """
    match = re.search(r'/fc-([^-]+)-[^/]*\.sock', socket_path)
    if match:
        return match.group(1)
    return None


def parse_sandbox_ids(input_source):
    """从输入中解析所有sandbox_id"""
    sandbox_ids = []
    
    if input_source == sys.stdin:
        lines = sys.stdin.readlines()
    else:
        with open(input_source, 'r') as f:
            lines = f.readlines()
    
    # 匹配 --api-sock 后面的socket路径
    socket_pattern = re.compile(r'--api-sock\s+([^\s]+\.sock)')
    
    for line in lines:
        matches = socket_pattern.findall(line)
        for socket_path in matches:
            sandbox_id = extract_sandbox_id(socket_path)
            if sandbox_id:
                sandbox_ids.append(sandbox_id)
    
    return sandbox_ids


def extract_node_ip(file_path):
    """从文件名中提取节点IP地址"""
    if file_path == "stdin":
        return None
    
    # 匹配IP地址格式的文件名，如 10.12.1.252.txt
    basename = os.path.basename(file_path)
    ip_pattern = re.match(r'^(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})\.txt$', basename)
    if ip_pattern:
        return ip_pattern.group(1)
    return None


def output_summary(file_path, total_count, unique_count):
    """输出简洁模式（只显示数量）"""
    node_ip = extract_node_ip(file_path)
    if node_ip:
        display_name = f"节点IP: {node_ip}"
    elif file_path == "stdin":
        display_name = "(stdin)"
    else:
        display_name = file_path
    print(f"{display_name:<50} Sandbox数量: {total_count:<6}")


def output_verbose(file_path, total_count, unique_count, unique_ids, duplicates):
    """输出详细模式（显示sandbox信息）"""
    node_ip = extract_node_ip(file_path)
    if node_ip:
        display_name = f"节点IP: {node_ip}"
        file_info = f"文件: {file_path}"
    elif file_path == "stdin":
        display_name = "(stdin)"
        file_info = "文件: (stdin)"
    else:
        display_name = file_path
        file_info = f"文件: {file_path}"
    
    print("=" * 50)
    if node_ip:
        print(f"{file_info}")
        print(f"{display_name}")
    else:
        print(f"{file_info}")
    print("=" * 50)
    print(f"Sandbox数量: {total_count}")
    print()
    
    if unique_count > 0:
        print("Sandbox ID列表:")
        print("-" * 40)
        for idx, sandbox_id in enumerate(unique_ids, 1):
            print(f"{idx:4d}. {sandbox_id}")
        print()
        
        if duplicates:
            print("注意: 发现重复的Sandbox ID:")
            for sandbox_id, count in sorted(duplicates.items()):
                print(f"  {sandbox_id}: {count} 次")
            print()
    print()


def output_json(file_path, total_count, unique_count, unique_ids, duplicates):
    """输出JSON格式"""
    result = {
        "file": file_path,
        "total_count": total_count,
        "unique_count": unique_count,
        "sandbox_ids": unique_ids,
        "duplicates": duplicates if duplicates else {}
    }
    print(json.dumps(result, indent=2, ensure_ascii=False))


def output_csv(file_path, unique_ids, is_first=False):
    """输出CSV格式"""
    if is_first:
        print("file,sandbox_id")
    for sandbox_id in unique_ids:
        print(f"{file_path},{sandbox_id}")


def output_list(file_path, unique_ids):
    """输出简单列表格式"""
    for sandbox_id in unique_ids:
        print(sandbox_id)


def process_file(file_path, verbose=False, output_format='table', is_first=False, is_last=False):
    """处理单个文件"""
    try:
        sandbox_ids = parse_sandbox_ids(file_path)
    except Exception as e:
        print(f"警告: 处理文件时出错 {file_path}: {e}", file=sys.stderr)
        return None
    
    total_count = len(sandbox_ids)
    unique_ids = sorted(set(sandbox_ids))
    unique_count = len(unique_ids)
    
    # 检查重复
    counter = Counter(sandbox_ids)
    duplicates = {sid: count for sid, count in counter.items() if count > 1}
    
    # 根据格式输出
    if output_format == 'json':
        output_json(file_path, total_count, unique_count, unique_ids, duplicates)
        if not is_last:
            print(",")
    elif output_format == 'csv':
        output_csv(file_path, unique_ids, is_first)
    elif output_format == 'list':
        output_list(file_path, unique_ids)
    else:  # table
        if verbose:
            output_verbose(file_path, total_count, unique_count, unique_ids, duplicates)
        else:
            output_summary(file_path, total_count, unique_count)
    
    return {
        'file': file_path,
        'total_count': total_count,
        'unique_count': unique_count,
        'unique_ids': unique_ids
    }


def main():
    parser = argparse.ArgumentParser(
        description='从firecracker进程信息中提取sandbox_id',
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
示例:
  # 从多个文件读取（简洁模式）
  python3 extract-sandbox-ids.py file1.txt file2.txt
  
  # 详细模式
  python3 extract-sandbox-ids.py --verbose file1.txt file2.txt
  
  # 从stdin读取
  ps aux | grep firecracker | python3 extract-sandbox-ids.py
  
  # JSON格式
  python3 extract-sandbox-ids.py --format json file1.txt file2.txt
  
  # CSV格式
  python3 extract-sandbox-ids.py --format csv file1.txt file2.txt > sandbox_ids.csv
        """
    )
    
    parser.add_argument(
        'files',
        nargs='*',
        default=None,
        help='输入文件路径（不提供则自动读取当前目录下所有*.txt文件，使用"-"表示从stdin读取）'
    )
    
    parser.add_argument(
        '-v', '--verbose',
        action='store_true',
        help='显示详细的sandbox信息（默认只显示数量）'
    )
    
    parser.add_argument(
        '--format',
        choices=['table', 'json', 'csv', 'list'],
        default='table',
        help='输出格式（默认: table）'
    )
    
    args = parser.parse_args()
    
    # 处理文件列表
    files_to_process = []
    
    # 如果没有提供文件参数，自动读取当前目录下的所有*.txt文件
    if not args.files or len(args.files) == 0:
        txt_files = sorted(glob.glob('*.txt'))
        if txt_files:
            files_to_process = [str(Path(f)) for f in txt_files]
            print(f"自动发现 {len(files_to_process)} 个txt文件", file=sys.stderr)
        else:
            # 如果没有找到txt文件，检查stdin是否有输入
            if sys.stdin.isatty():
                print("错误: 没有提供文件参数，且当前目录下没有*.txt文件", file=sys.stderr)
                print("提示: 可以指定文件，或使用管道输入: ps aux | grep firecracker | python3 extract-sandbox-ids.py", file=sys.stderr)
                sys.exit(1)
            else:
                files_to_process = ['stdin']
    else:
        for file_arg in args.files:
            if file_arg == '-' or file_arg == '/dev/stdin':
                files_to_process.append('stdin')
            else:
                file_path = Path(file_arg)
                if not file_path.exists():
                    print(f"警告: 文件不存在，跳过: {file_arg}", file=sys.stderr)
                    continue
                files_to_process.append(str(file_path))
    
    if not files_to_process:
        print("错误: 没有有效的输入文件", file=sys.stderr)
        sys.exit(1)
    
    # 处理JSON格式的特殊情况（需要数组格式）
    if args.format == 'json' and len(files_to_process) > 1:
        print("[")
    
    # 处理每个文件
    results = []
    for idx, file_path in enumerate(files_to_process):
        result = process_file(
            file_path,
            verbose=args.verbose,
            output_format=args.format,
            is_first=(idx == 0),
            is_last=(idx == len(files_to_process) - 1)
        )
        if result:
            results.append(result)
    
    # JSON格式收尾
    if args.format == 'json' and len(files_to_process) > 1:
        print("]")
    
    # 显示汇总信息（仅在table格式且非verbose模式）
    if args.format == 'table' and not args.verbose and len(results) > 0:
        total_files = len(results)
        total_sandboxes = sum(r['total_count'] for r in results)
        
        print()
        print("=" * 50)
        print(f"{'总计: ' + str(total_files) + ' 个节点':<50} Sandbox数量: {total_sandboxes:<6}")
        print("=" * 50)


if __name__ == '__main__':
    main()

