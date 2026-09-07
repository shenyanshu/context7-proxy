// 原生 fetch 的薄封装：统一鉴权头、错误提取、401 全局跳登出
// 不引 axios——契约简单，一层封装足够

export const KEY_STORAGE = 'c7p_master_key';
/** HashRouter 下登录页地址常量，401 时整页跳转到这里 */
export const LOGIN_PATH = '#/login';

export class ApiError extends Error {
  status: number;

  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T | null> {
  const key = localStorage.getItem(KEY_STORAGE);
  const res = await fetch(path, {
    ...init,
    headers: {
      // 只有带 body 的请求才需要 JSON Content-Type，GET 带上也无害但没必要
      ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
      ...(key ? { Authorization: `Bearer ${key}` } : {}),
    },
  });

  if (res.status === 401) {
    // 401 意味着 master key 失效或错误，丢弃本地凭证回到登录页
    localStorage.removeItem(KEY_STORAGE);
    window.location.assign(LOGIN_PATH);
    throw new ApiError(401, '未授权，请重新登录');
  }

  if (!res.ok) {
    const message = await extractError(res);
    throw new ApiError(res.status, message);
  }

  // 后端契约里 204 可能无 body，直接返回 null
  if (res.status === 204) {
    return null;
  }
  return (await res.json()) as T;
}

/** 错误统一约定为 {"error":"..."}，解析失败时回退到状态文案 */
async function extractError(res: Response): Promise<string> {
  try {
    const body = (await res.json()) as { error?: string };
    if (body.error) {
      return body.error;
    }
  } catch {
    // 非 JSON 错误体（如网关默认报错页），忽略并走状态文案
  }
  return `请求失败（${res.status}）`;
}

export const api = {
  get: <T>(path: string) => request<T>(path),
  post: <T>(path: string, body: unknown) =>
    request<T>(path, { method: 'POST', body: JSON.stringify(body) }),
  patch: <T>(path: string, body: unknown) =>
    request<T>(path, { method: 'PATCH', body: JSON.stringify(body) }),
  delete: <T>(path: string) => request<T>(path, { method: 'DELETE' }),
};
