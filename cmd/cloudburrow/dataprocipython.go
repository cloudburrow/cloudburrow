package main

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// IPython for PySpark notebook batches (#2162): IPython 8.37.0 (the last
// IPython line on Python 3.10, the runtimes' Spark 3.5 images' Python) and
// its dependencies, each its pure-Python wheel from PyPI pinned by
// SHA-256 in dependencies.json (components.bigqueryml.dataprocIPython),
// unpacked into one directory that a notebook batch's driver has on its
// PYTHONPATH. Their licences are BSD-3-Clause (IPython, matplotlib-inline,
// prompt_toolkit, traitlets), BSD-2-Clause (decorator, Pygments), MIT
// (exceptiongroup, jedi, parso, stack_data, executing, pure_eval,
// wcwidth), ISC (pexpect, ptyprocess), Apache-2.0 (asttokens) and
// PSF-2.0 (typing_extensions). Nothing of them is in the CLI or the
// repository.
type pyWheel struct{ url, sha256 string }

const pythonhosted = "https://files.pythonhosted.org/packages/"

var ipythonWheels = []pyWheel{
	{"91/d0/274fbf7b0b12643cbbc001ce13e6a5b1607ac4929d1b11c72460152c9fc3/ipython-8.37.0-py3-none-any.whl", "ed87326596b878932dbcb171e3e698845434d8c61b8d8cd474bf663041a9dcf2"},
	{"4e/8c/f3147f5c4b73e7550fe5f9352eaa956ae838d5c51eb58e7a25b9f3e2643b/decorator-5.2.1-py3-none-any.whl", "d316bb415a2d9e2d2b3abcc4084c6502fc09240e292cd76a76afc106a1c8e04a"},
	{"36/f4/c6e662dade71f56cd2f3735141b265c3c79293c109549c1e6933b0651ffc/exceptiongroup-1.3.0-py3-none-any.whl", "4d111e6e0c13d0644cad6ddaa7ed0261a0b36971f6d23e7ec9b4b9097da78a10"},
	{"c0/5a/9cac0c82afec3d09ccd97c8b6502d48f165f9124db81b4bcb90b4af974ee/jedi-0.19.2-py2.py3-none-any.whl", "a8ef22bde8490f57fe5c7681a3c83cb58874daf72b4784de3cce5b6ef6edb5b9"},
	{"16/32/f8e3c85d1d5250232a5d3477a2a28cc291968ff175caeadaf3cc19ce0e4a/parso-0.8.5-py2.py3-none-any.whl", "646204b5ee239c396d040b90f9e272e9a8017c630092bf59980beb62fd033887"},
	{"af/33/ee4519fa02ed11a94aef9559552f3b17bb863f2ecfe1a35dc7f548cde231/matplotlib_inline-0.2.1-py3-none-any.whl", "d56ce5156ba6085e00a9d54fead6ed29a9c47e215cd1bba2e976ef39f5710a76"},
	{"9e/c3/059298687310d527a58bb01f3b1965787ee3b40dce76752eda8b44e9a2c5/pexpect-4.9.0-py2.py3-none-any.whl", "7236d1e080e4936be2dc3e326cec0af72acf9212a7e1d060210e70a47e253523"},
	{"22/a6/858897256d0deac81a172289110f31629fc4cee19b6f01283303e18c8db3/ptyprocess-0.7.0-py2.py3-none-any.whl", "4b41f3967fce3af57cc7e94b888626c18bf37a083e3651ca8feeb66d492fef35"},
	{"84/03/0d3ce49e2505ae70cf43bc5bb3033955d2fc9f932163e84dc0779cc47f48/prompt_toolkit-3.0.52-py3-none-any.whl", "9aac639a3bbd33284347de5ad8d68ecc044b91a762dc39b7c21095fcd6a19955"},
	{"c7/21/705964c7812476f378728bdf590ca4b771ec72385c533964653c68e86bdc/pygments-2.19.2-py3-none-any.whl", "86540386c03d588bb81d44bc3928634ff26449851e99741617ecb9037ee5ec0b"},
	{"f1/7b/ce1eafaf1a76852e2ec9b22edecf1daa58175c090266e9f6c64afcd81d91/stack_data-0.6.3-py3-none-any.whl", "d5558e0c25a4cb0853cddad3d77da9891a08cb85dd9f9f91b9f8cd66e511e695"},
	{"c1/ea/53f2148663b321f21b5a606bd5f191517cf40b7072c0497d3c92c4a13b1e/executing-2.2.1-py2.py3-none-any.whl", "760643d3452b4d777d295bb167ccc74c64a81df23fb5e08eff250c425a4b2017"},
	{"25/8a/c46dcc25341b5bce5472c718902eb3d38600a903b14fa6aeecef3f21a46f/asttokens-3.0.0-py3-none-any.whl", "e3078351a059199dd5138cb1c706e6430c05eff2ff136af5eb4790f9d28932e2"},
	{"8e/37/efad0257dc6e593a18957422533ff0f87ede7c9c6ea010a2177d738fb82f/pure_eval-0.2.3-py3-none-any.whl", "1db8e35b67b3d218d818ae653e27f06c3aa420901fa7b081ca98cbedc874e0d0"},
	{"00/c0/8f5d070730d7836adc9c9b6408dec68c6ced86b304a9b26a14df072a6e8c/traitlets-5.14.3-py3-none-any.whl", "b74e89e397b1ed28cc831db7aea759ba6640cb3de13090ca145426688ff1ac4f"},
	{"18/67/36e9267722cc04a6b9f15c7f3441c2363321a3ea07da7ae0c0707beb2a9c/typing_extensions-4.15.0-py3-none-any.whl", "f0fa19c6845758ab08074a0cfa8b7aecb71c999ca73d62883bc25cc018c4e548"},
	{"af/b5/123f13c975e9f27ab9c0770f514345bd406d0e8d3b7a0723af9d43f710af/wcwidth-0.2.14-py2.py3-none-any.whl", "a7bb560c8aee30f9957e5f9895805edd20602f2d7f720186dfd906e82b4982e1"},
}

// ipythonHome is the directory of the unpacked wheels in cache, fetched,
// checked and unpacked on the first use.
func ipythonHome(cache string, out io.Writer) func(ctx context.Context) (string, error) {
	var mu sync.Mutex
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		home := filepath.Join(cache, "ipython-8.37.0")
		marker := filepath.Join(home, ".complete")
		if _, err := os.Stat(marker); err == nil {
			return home, nil
		}
		_ = os.RemoveAll(home)
		if err := os.MkdirAll(home, 0o755); err != nil {
			return "", err
		}
		for _, w := range ipythonWheels {
			name := filepath.Base(w.url)
			p := filepath.Join(cache, "wheels", name)
			if _, err := fetchPinned(ctx, p, pythonhosted+w.url, w.sha256, name, out); err != nil {
				return "", err
			}
			if err := unzipInto(p, home); err != nil {
				return "", fmt.Errorf("%s: %w", name, err)
			}
		}
		return home, os.WriteFile(marker, nil, 0o644)
	}
}

// unzipInto extracts a wheel's files into dir, refusing paths that
// leave it.
func unzipInto(wheel, dir string) error {
	r, err := zip.OpenReader(wheel)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		name := filepath.FromSlash(f.Name)
		if strings.Contains(f.Name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("the wheel has a file %q outside its root", f.Name)
		}
		dst := filepath.Join(dir, name)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		w, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(w, io.LimitReader(rc, 64<<20))
		rc.Close()
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}
