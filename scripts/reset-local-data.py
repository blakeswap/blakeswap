#!/usr/bin/env python3
"""Reset the active desktop installation, retaining a recoverable data archive."""
import argparse
import datetime
import fcntl
import os
import pathlib
import uuid


def reset(data_dir):
    supplied = pathlib.Path(data_dir).expanduser()
    if not supplied.is_absolute() or supplied.is_symlink():
        raise ValueError('Use an absolute app data directory, not a symlink.')
    root = supplied.resolve()
    if root in (pathlib.Path('/'), pathlib.Path.home(), pathlib.Path.cwd(), pathlib.Path('/tmp').resolve()):
        raise ValueError('Refusing to reset this directory.')
    if not root.exists():
        return None
    if not root.is_dir() or not (root / 'settings.json').is_file():
        raise ValueError('Directory does not contain Blakeswap settings.json.')
    descriptor = os.open(root / 'desktop.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise ValueError('Quit Blakeswap before resetting local storage.') from error
        timestamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
        archive = root.with_name(root.name + '-backup-' + timestamp + '-' + uuid.uuid4().hex[:8])
        root.rename(archive)
        return archive
    finally:
        os.close(descriptor)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--data-dir', required=True)
    args = parser.parse_args()
    try:
        archive = reset(args.data_dir)
    except (ValueError, OSError) as error:
        parser.exit(1, str(error) + '\n')
    if archive:
        print('Local storage reset. Onboarding will appear on the next app launch.')
        print('Previous wallet data is preserved at: ' + str(archive))
    else:
        print('No local storage exists. Onboarding will appear on the next app launch.')


if __name__ == '__main__':
    main()
