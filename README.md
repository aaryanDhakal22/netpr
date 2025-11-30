# 🖨️ Netpr — Remote Printing From Anywhere  
A secure, containerized remote-printing system built for home servers and small networks.

Netpr allows users to upload documents through a simple web interface and print them directly to a printer connected to your home server — even when you're away from the local network.

This project powers my personal remote printing service.  
It’s designed with security, simplicity, and portability in mind.

## 🚀 Features

### ✔️ Remote Printing  
Upload a document (PDF, PNG, JPG, DOCX → auto-converted) and print it instantly via your server-connected printer.

### ✔️ Minimal Web UI  
A lightweight interface for fast uploads — no client installation required.

### ✔️ CUPS Integration  
Uses the underlying **CUPS** printing system, allowing compatibility with virtually any printer configured on the server.

### ✔️ Secure by Design  
- HTTPS-enabled through Traefik  
- Deployment on isolated VLAN  
- Runs in non-privileged containers  
- Optional Tailscale / Cloudflare Access protection  

### ✔️ Containerized  
Fully packaged for Podman deployments.

---

## 🏗️ Architecture Overview

Client → Web UI → Backend API → Local CUPS Server → Printer

**Frontend**
- Static HTML/JS  
- File selection and upload interface  

**Backend**
- Python API (FastAPI/Flask)  
- Accepts uploads, validates, converts if necessary  
- Sends print job to CUPS (`lp` or `pycups`)  

**Infrastructure**
- Traefik reverse proxy  
- Auto-SSL  
- Podman rootless containers  
- Cloudflare DNS  

---

** 🧭 Roadmap

- [ ] Add authentication
- [ ] Print queue viewer
- [ ] Multi-printer support
- [ ] Improved mobile UI
- [ ]PDF preview before printing

🤝 Contributions
Pull requests are welcome!
Feel free to open an issue for ideas, bugs, or improvements.
