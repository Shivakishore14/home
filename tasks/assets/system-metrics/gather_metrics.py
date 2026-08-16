#!/usr/bin/env python3
import os
import sys
import platform
import shutil

print("=== TaskEngine System Metrics Reporter ===")
print("PROGRESS: 25%")

print(f"OS: {platform.system()} {platform.release()} ({platform.machine()})")
print(f"Node Hostname: {platform.node()}")
print("PROGRESS: 50%")

total, used, free = shutil.disk_usage("/")
print(f"Disk Usage (/): {used // (2**30)}GB used / {total // (2**30)}GB total ({free // (2**30)}GB free)")
print("PROGRESS: 80%")

print("Task Directory:", os.environ.get("TASK_DIR", "N/A"))
print("Assets Directory:", os.environ.get("TASK_ASSETS_DIR", "N/A"))
print("PROGRESS: 100%")
print("Metrics successfully collected!")
