import json, sys, numpy as np, xgboost as xgb, os
out = sys.argv[1]
rng = np.random.default_rng(7)
n = 200
X = rng.normal(size=(n, 3)).astype(np.float32)
X[rng.random((n,3)) < 0.1] = np.nan
cat = rng.integers(0, 4, size=n).astype(np.float32)
y_reg = 2*np.nan_to_num(X[:,0]) - np.nan_to_num(X[:,1]) + 0.5*np.nan_to_num(X[:,2]) + (cat==2)*1.5
y_bin = (y_reg > 0).astype(np.float32)
y_mc = np.digitize(y_reg, [-1, 1]).astype(np.float32)
test = rng.normal(size=(12, 3)).astype(np.float32)
test[1,0] = np.nan; test[5,2] = np.nan; test[7,:] = np.nan
tcat = np.array([0,1,2,3,0,1,2,3,0,1,2,3], dtype=np.float32)
fn3 = ['f_a','f_b','f_c']
cases = {}
def run(name, params, y, X_, T_, rounds=8, names=fn3, types=None, enable_cat=False):
    d = xgb.DMatrix(X_, label=y, feature_names=names, feature_types=types, enable_categorical=enable_cat)
    b = xgb.train(params, d, rounds)
    for ext in ('json','ubj'):
        b.save_model(os.path.join(out, name+'.'+ext))
    dt = xgb.DMatrix(T_, feature_names=names, feature_types=types, enable_categorical=enable_cat)
    p = b.predict(dt)
    imp = {k: [b.get_score(importance_type=k)] for k in ('weight','gain','cover')} if params.get('booster') != 'gblinear' else {}
    cases[name] = {'test': [[None if np.isnan(v) else float(v) for v in r] for r in T_], 'pred': p.tolist(), 'importance': imp}
run('reg', {'objective':'reg:squarederror','max_depth':3,'seed':1}, y_reg, X, test)
run('bin', {'objective':'binary:logistic','max_depth':3,'seed':1}, y_bin, X, test)
run('mc', {'objective':'multi:softprob','num_class':3,'max_depth':2,'seed':1}, y_mc, X, test, rounds=4)
run('mcmax', {'objective':'multi:softmax','num_class':3,'max_depth':2,'seed':1}, y_mc, X, test, rounds=4)
run('pois', {'objective':'count:poisson','max_depth':2,'seed':1}, np.abs(y_reg).round(), X, test)
run('dart', {'booster':'dart','objective':'reg:squarederror','max_depth':2,'rate_drop':0.3,'seed':1}, y_reg, X, test)
run('lin', {'booster':'gblinear','objective':'reg:squarederror','seed':1}, y_reg, X, test, rounds=5)
Xc = np.column_stack([X, cat]); Tc = np.column_stack([test, tcat])
run('cat', {'objective':'reg:squarederror','max_depth':3,'seed':1,'tree_method':'hist','max_cat_to_onehot':1}, y_reg, Xc, Tc, names=fn3+['f_cat'], types=['q','q','q','c'], enable_cat=True)
d = xgb.DMatrix(X, label=y_reg)
b = xgb.train({'objective':'reg:squarederror','max_depth':2}, d, 3)
b.save_model(os.path.join(out,'noname.json'))
cases['noname'] = {'test': [[None if np.isnan(v) else float(v) for v in r] for r in test], 'pred': b.predict(xgb.DMatrix(test)).tolist(), 'importance': {}}
json.dump(cases, open(os.path.join(out,'cases.json'),'w'), indent=1)
