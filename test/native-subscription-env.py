#!/usr/bin/env python3
"""Copy one literal subscription URL from a private dotenv file; never evaluate it."""
import os
import re
import stat
import sys
from urllib.parse import urlsplit

KEY = "VPNKIT_LAB_SUBSCRIPTION_URL"
MAX_ENV = 1024 * 1024


def extract(data):
    try:
        text = data.decode("utf-8")
    except UnicodeError:
        raise ValueError("dotenv must be UTF-8") from None
    values = []
    for line in text.splitlines():
        match = re.match(r"^[ \t]*(?:export[ \t]+)?" + KEY + r"[ \t]*=(.*)$", line)
        if not match:
            continue
        value = match.group(1).strip()
        if value.startswith(("'", '"')):
            quote = value[0]
            end = value.find(quote, 1)
            if end < 0 or (value[end + 1:].strip() and not value[end + 1:].lstrip().startswith("#")):
                raise ValueError("invalid quoted subscription setting")
            value = value[1:end]
        else:
            value = re.split(r"[ \t]+#", value, maxsplit=1)[0].strip()
        values.append(value)
    if len(values) != 1:
        raise ValueError("dotenv must contain exactly one VPNKIT_LAB_SUBSCRIPTION_URL")
    value = values[0]
    if not value or len(value) > 8192 or any(c.isspace() or ord(c) < 32 or ord(c) == 127 for c in value):
        raise ValueError("invalid subscription URL")
    try:
        parsed = urlsplit(value)
        valid = parsed.scheme == "https" and parsed.hostname and not parsed.username and not parsed.password
        valid = valid and not parsed.fragment and "\\" not in value
        _ = parsed.port
    except ValueError:
        valid = False
    if not valid:
        raise ValueError("subscription must be an absolute HTTPS URL without userinfo or fragment")
    return value


def copy_subscription(source, destination):
    fd = os.open(source, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC)
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise ValueError("dotenv must be an owned private regular file (chmod 600)")
        data = stream.read(MAX_ENV + 1)
    if len(data) > MAX_ENV:
        raise ValueError("dotenv exceeds size limit")
    value = extract(data)
    parent, leaf = os.path.split(os.path.abspath(destination))
    directory = os.open(parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC)
    try:
        info = os.fstat(directory)
        if info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise ValueError("subscription output directory must be private")
        output = os.open(leaf, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600, dir_fd=directory)
        try:
            with os.fdopen(output, "wb") as stream:
                stream.write((value + "\n").encode("utf-8"))
                stream.flush()
                os.fsync(stream.fileno())
        except BaseException:
            os.unlink(leaf, dir_fd=directory)
            raise
    finally:
        os.close(directory)


def main():
    if len(sys.argv) != 3:
        print("usage: native-subscription-env.py ENVFILE OUTPUTFILE", file=sys.stderr)
        return 2
    try:
        copy_subscription(sys.argv[1], sys.argv[2])
    except ValueError as error:
        # All ValueError messages are fixed diagnostics, never source contents.
        print("blocker: " + str(error), file=sys.stderr)
        return 2
    except OSError:
        print("blocker: cannot securely read dotenv or create private subscription file", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
