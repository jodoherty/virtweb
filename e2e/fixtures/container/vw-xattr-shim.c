/* vw-xattr-shim.c — LD_PRELOAD shim for the e2e libvirt container.
 *
 * libvirt's security (DAC) driver labels devices and private files:
 * trusted.* xattrs, POSIX ACLs, chown, chmod on console pty /
 * /dev/random / private XML / master key. Two of those are impossible
 * in a rootless podman container:
 *   - trusted.* xattrs require CAP_SYS_ADMIN in the init user
 *     namespace (kernel check);
 *   - host device nodes bind-mounted into /dev cannot be chowned /
 *     chmod'ed / ACL'ed by the container user.
 *
 * Both are meaningless in the e2e fake host, so this shim no-ops them:
 *   - setxattr/lsetxattr/fsetxattr of "trusted.libvirt.*" anywhere,
 *   - setxattr/lsetxattr/fsetxattr, chown/lchown/fchown, chmod/fchmod
 *     of any path under /dev/.
 * Everything else is passed through to the real syscall.
 *
 * Build: gcc -shared -fPIC -O2 -o vw-xattr-shim.so vw-xattr-shim.c -ldl
 */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>
#include <sys/stat.h>
#include <sys/types.h>

#define VW_PREFIX "trusted.libvirt"

static int
vw_is_trusted_name(const char *name)
{
	return name && strncmp(name, VW_PREFIX, strlen(VW_PREFIX)) == 0;
}

/* True for any path the DAC driver labels on the host (device nodes
 * under /dev/). f* variants resolve the path via /proc/self/fd. */
static int
vw_is_dev_path(const char *path, int fd)
{
	char tmp[64];
	const char *p = path;
	if (!p && fd >= 0) {
		if (snprintf(tmp, sizeof(tmp), "/proc/self/fd/%d", fd)
		    >= (int)sizeof(tmp))
			return 0;
		if (readlink(tmp, tmp, sizeof(tmp)) < 0)
			return 0;
		p = tmp;
	}
	return p && strncmp(p, "/dev/", 5) == 0;
}

int
setxattr(const char *path, const char *name, const void *value,
	 size_t size, int flags)
{
	static int (*real)(const char *, const char *, const void *,
			   size_t, int) = NULL;
	if (!real)
		real = dlsym(RTLD_NEXT, "setxattr");
	if (vw_is_trusted_name(name) || vw_is_dev_path(path, -1))
		return 0;
	return real(path, name, value, size, flags);
}

int
lsetxattr(const char *path, const char *name, const void *value,
	  size_t size, int flags)
{
	static int (*real)(const char *, const char *, const void *,
			   size_t, int) = NULL;
	if (!real)
		real = dlsym(RTLD_NEXT, "lsetxattr");
	if (vw_is_trusted_name(name) || vw_is_dev_path(path, -1))
		return 0;
	return real(path, name, value, size, flags);
}

int
fsetxattr(int fd, const char *name, const void *value, size_t size, int flags)
{
	static int (*real)(int, const char *, const void *, size_t, int) =
		NULL;
	if (!real)
		real = dlsym(RTLD_NEXT, "fsetxattr");
	if (vw_is_trusted_name(name) || vw_is_dev_path(NULL, fd))
		return 0;
	return real(fd, name, value, size, flags);
}

int
chown(const char *path, uid_t uid, gid_t gid)
{
	static int (*real)(const char *, uid_t, gid_t) = NULL;
	if (!real)
		real = dlsym(RTLD_NEXT, "chown");
	if (vw_is_dev_path(path, -1))
		return 0;
	return real(path, uid, gid);
}

int
lchown(const char *path, uid_t uid, gid_t gid)
{
	static int (*real)(const char *, uid_t, gid_t) = NULL;
	if (!real)
		real = dlsym(RTLD_NEXT, "lchown");
	if (vw_is_dev_path(path, -1))
		return 0;
	return real(path, uid, gid);
}

int
fchown(int fd, uid_t uid, gid_t gid)
{
	static int (*real)(int, uid_t, gid_t) = NULL;
	if (!real)
		real = dlsym(RTLD_NEXT, "fchown");
	if (vw_is_dev_path(NULL, fd))
		return 0;
	return real(fd, uid, gid);
}

int
chmod(const char *path, mode_t mode)
{
	static int (*real)(const char *, mode_t) = NULL;
	if (!real)
		real = dlsym(RTLD_NEXT, "chmod");
	if (vw_is_dev_path(path, -1))
		return 0;
	return real(path, mode);
}

int
fchmod(int fd, mode_t mode)
{
	static int (*real)(int, mode_t) = NULL;
	if (!real)
		real = dlsym(RTLD_NEXT, "fchmod");
	if (vw_is_dev_path(NULL, fd))
		return 0;
	return real(fd, mode);
}
