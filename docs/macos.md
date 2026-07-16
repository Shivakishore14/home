# macOS Host Configuration & Considerations

This document provides system administration instructions for configuring a macOS host (specifically Apple Silicon Mac mini) running Docker Desktop for this home server stack.

---

## 1. Docker Desktop Installation

To set up the server:
1. Download **Docker Desktop for Mac with Apple Silicon** from the [Official Docker Website](https://www.docker.com/products/docker-desktop/).
2. Drag and drop the Docker application to your `Applications` folder.
3. Open `Docker.app` and complete the initial setup wizard.

---

## 2. macOS Security & Drive Permissions

macOS implements strict sandboxing and security controls. You must explicitly grant permissions to Docker to access external storage and system commands.

### File Sharing / Volume Access
By default, Docker Desktop can only access `/Users`, `/Volume` sharing needs to be enabled:
1. Open Docker Desktop.
2. Go to **Settings (Gear Icon) > Resources > File sharing**.
3. Add the path `/Volumes/drive001` or `/Volumes` to the list of shared paths.
4. Click **Apply & restart**.

### System Access Permissions
To ensure Docker can mount and manage files without sandboxing limits, grant the following in **System Settings > Privacy & Security**:
- **Files and Folders**: Verify `Docker` has access to the external drive `/Volumes/drive001` (or "Removable Volumes").
- **Full Disk Access** (Optional but recommended for headless/home server setups): Add `Docker` to the list to prevent execution errors when running scripts via automation schedulers (like cron).

---

## 3. External Drive Filesystem Considerations

### Filesystem Formats
Ensure your external drive (`/Volumes/drive001`) is formatted with a modern filesystem:
- **APFS (Apple File System)**: **Strongly Recommended**. Optimized for SSDs, supports copy-on-write, and shares permissions correctly with Docker Desktop's VirtioFS framework.
- **exFAT**: Avoid if possible. It does not support native UNIX file permissions, symlinks, or file locks, which can cause SQLite databases (used by Jellyfin and Home Assistant) to crash or suffer corruption.

### Persistent Mount Points
In macOS, external drives are mounted dynamically under `/Volumes/<DriveName>`. If the drive is unplugged and plugged back in, macOS might append a number (e.g., `/Volumes/drive001 1`).
- **Fix**: To ensure paths are persistent, configure a static mount point or check your drive names in Disk Utility. Ensure the name matches the `DOCKER_DIR` and `MEDIA_DIR` variables in your `.env` file exactly.

---

## 4. Hardware Acceleration on Apple Silicon

### Jellyfin Transcoding
Running Jellyfin inside a Linux Docker container on macOS **does not support hardware acceleration via Apple Silicon's VideoToolbox media engines**.
- **Reason**: Docker on macOS runs inside a Linux virtual machine. Because of macOS's virtualization framework limitations, the host GPU is not passed through to the Linux guest. Standard GPU device mappings (`/dev/dri`) will fail.
- **Impact**: Transcoding in Jellyfin will run on the CPU (software encoding). While Apple Silicon cores are powerful enough to transcode 1080p and some 4K streams on CPU, it will increase CPU utilization and heat.
- **Alternative**: If you require dedicated hardware transcoding, you should install the **native macOS Jellyfin server binary** directly on the host rather than running it inside Docker.

---

## 5. Home Assistant USB Passthrough (Zigbee / Z-Wave)

Directly mapping USB serial devices (e.g., `/dev/ttyUSB0` or `/dev/ttyACM0`) using the `devices:` block in Docker Compose **is not natively supported on macOS**.

### Recommended Solutions
1. **USB/IP (Docker Desktop Feature)**:
   You can utilize the USB/IP protocol to map USB devices from macOS host to the Docker Linux VM.
   - Install `usbipd` equivalent on macOS or follow Docker's official guide on [Docker USB/IP Features](https://docs.docker.com/desktop/features/usbip/).
2. **Network/IP-Based Bridges (Highly Recommended)**:
   Avoid physical USB dongles connected to the Mac mini. Instead, use network-attached coordinators:
   - **Zigbee**: Use a LAN-based Zigbee coordinator (e.g., SLZB-06, Sonoff ZBDongle-E flashed with firmware supporting PoE/Ethernet, or Tube's Zigbee Gateway).
   - This allows Mosquitto/Zigbee2MQTT to run in Docker and fetch data over the local network rather than relying on USB passthrough.
3. **Run Home Assistant OS (HAOS) in a Virtual Machine**:
   If USB pass-through is a hard requirement, run Home Assistant OS inside a virtual machine manager like **UTM** or **VMware Fusion** on Apple Silicon. This supports direct USB hardware passthrough to the guest operating system.
