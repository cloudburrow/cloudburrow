"""Writes internal/adapter/run/ssmtemplates.tar.gz, Secure Source Manager's
repository initialization templates (#2071), from github/gitignore and
spdx/license-list-data checkouts (third_party/ssm-templates/PROVENANCE.md).

    python3 -I tools/ssmtemplates/generate.py PB_GO GITIGNORE_DIR SPDX_TEXT_DIR OUT

PB_GO is the pinned securesourcemanagerpb/secure_source_manager.pb.go, whose
InitialConfig comments list the template names Google accepts. Each name maps
to the github/gitignore file or SPDX license text it names: the CamelCase
file name in kebab case (CMake is c-make, Objective-C objective--c), an SPDX
ID starting with a digit prefixed license-, and the exceptions below. The
archive holds gitignore/NAME and license/NAME, sorted, with no timestamps.
"""
import gzip, io, os, re, sys, tarfile

GITIGNORE_EXCEPTIONS = {
    'cpp': 'C++.gitignore', 'drupal-7': 'community/PHP/Drupal7.gitignore', 'epi-server': 'EPiServer.gitignore',
    'jboss': 'JBoss.gitignore', 'jboss-4': 'community/Java/JBoss4.gitignore', 'jboss-6': 'community/Java/JBoss6.gitignore',
    'jdeveloper': 'Global/JDeveloper.gitignore', 'jenv': 'Global/JEnv.gitignore', 'kdevelop4': 'Global/KDevelop4.gitignore',
    'lyx': 'Global/LyX.gitignore', 'magento-1': 'community/PHP/Magento1.gitignore', 'magento-2': 'community/PHP/Magento2.gitignore',
    'nwjs': 'community/JavaScript/NWjs.gitignore', 'objective--c': 'Objective-C.gitignore', 'ocaml': 'OCaml.gitignore',
    'openssl': 'community/OpenSSL.gitignore', 'perl6': 'Raku.gitignore', 'psoc-creator': 'Global/PSoCCreator.gitignore',
    'putty': 'Global/PuTTY.gitignore', 'scons': 'SCons.gitignore', 'tex': 'TeX.gitignore', 'turbo-gears-2': 'TurboGears2.gitignore',
}


def names(pb):
    lines = open(pb).read().split('\n')
    out = []
    for i, line in enumerate(lines):
        if 'Valid values:' not in line:
            continue
        text = ''
        while lines[i].strip().startswith('//'):
            text += ' ' + lines[i].strip()[2:]
            i += 1
        out.append([v.strip().rstrip('.') for v in text.split('Valid values:', 1)[1].split(',') if v.strip()])
    return out[0], out[1]


def kebab(n):
    s = re.sub(r'(?<=[a-z0-9])(?=[A-Z])', '-', n)
    s = re.sub(r'(?<=[A-Z])(?=[A-Z][a-z])', '-', s)
    return s.replace('_', '-').replace('.', '-').lower()


def main(pb, gi, spdx, out):
    gnames, lnames = names(pb)
    files = {}
    for root, _, fs in os.walk(gi):
        for f in fs:
            if f.endswith('.gitignore'):
                rel = os.path.relpath(os.path.join(root, f), gi)
                pri = 0 if os.sep not in rel else (1 if rel.startswith('Global') else 2)
                k = kebab(f[:-len('.gitignore')])
                if k not in files or files[k][0] > pri:
                    files[k] = (pri, rel)
    lic = {}
    for f in sorted(os.listdir(spdx)):
        if not f.endswith('.txt'):
            continue
        sid = f[:-4]
        dep = sid.startswith('deprecated_')
        sid = sid.replace('deprecated_', '')
        for k in {sid.lower().replace('.', '-').replace('+', '-plus'), kebab(sid)}:
            if k[0].isdigit():
                k = 'license-' + k
            if k not in lic or (lic[k].startswith('deprecated_') and not dep):
                lic[k] = f
    entries = []
    for n in gnames:
        src = GITIGNORE_EXCEPTIONS.get(n) or files[n][1]
        entries.append(('gitignore/' + n, open(os.path.join(gi, src), 'rb').read()))
    for n in lnames:
        entries.append(('license/' + n, open(os.path.join(spdx, lic[n]), 'rb').read()))
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode='w', format=tarfile.USTAR_FORMAT) as t:
        for name, data in sorted(entries):
            ti = tarfile.TarInfo(name)
            ti.size, ti.mtime, ti.mode = len(data), 0, 0o644
            t.addfile(ti, io.BytesIO(data))
    with open(out, 'wb') as f:
        f.write(gzip.compress(buf.getvalue(), compresslevel=9, mtime=0))
    print(len(gnames), 'gitignores,', len(lnames), 'licenses')


if __name__ == '__main__':
    main(*sys.argv[1:5])
