"""Drive the real installer in a PTY; retain terminal output privately."""
import fcntl
import os
import pty
import select
import signal
import struct
import termios
import time
from pathlib import Path

pid, fd = pty.fork()
if pid == 0:
    os.environ.update(TERM="xterm-256color", LANG="C.UTF-8")
    os.execv("./install.sh", ["./install.sh"])
fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 32, 110, 0, 0))
Path(".build").mkdir(exist_ok=True, mode=0o700)
started = done = failed = reaped = False
buffer = b""
deadline = time.monotonic() + 2400
try:
    with open(".build/install-lab-terminal.log", "wb") as log:
        while time.monotonic() < deadline:
            if select.select([fd], [], [], 1)[0]:
                try:
                    chunk = os.read(fd, 65536)
                except OSError:
                    break
                if not chunk:
                    break
                log.write(chunk)
                log.flush()
                buffer = (buffer + chunk)[-262144:]
                if b"\x1b[6n" in chunk:
                    os.write(fd, b"\x1b[1;1R")
                text = buffer.decode("utf-8", errors="replace")
                if not started and "Одна установка" in text:
                    os.write(fd, b"\r")
                    started = True
                    buffer = b""
                elif started and not done and not failed:
                    if "Всё подготовлено" in text:
                        done = True
                        os.write(fd, b"q")
                    elif "Установка не завершена" in text:
                        failed = True
                        os.write(fd, b"q")
            child, status = os.waitpid(pid, os.WNOHANG)
            if child:
                reaped = True
                assert os.waitstatus_to_exitcode(status) == 0
                break
        assert done and not failed, "real installer did not complete; inspect private terminal log"
finally:
    if not reaped:
        try:
            os.kill(pid, signal.SIGTERM)
            os.waitpid(pid, 0)
        except ProcessLookupError:
            pass
    os.close(fd)
