// 纯展示格式化工具，与具体组件解耦，便于两页复用
import dayjs from 'dayjs';
import relativeTime from 'dayjs/plugin/relativeTime';
import 'dayjs/locale/zh-cn';

dayjs.extend(relativeTime);
dayjs.locale('zh-cn');

/** 相对时间展示（如 "3 分钟前"），null 视为从未发生 */
export function fromNow(unixSec: number | null): string {
  return unixSec ? dayjs.unix(unixSec).fromNow() : '从未';
}

/** 表格用的具体时刻 */
export function fullTime(unixSec: number): string {
  return dayjs.unix(unixSec).format('HH:mm:ss');
}

/** 冷却倒计时，单位秒；非正数截断为 0，由调用方决定是否渲染 */
export function formatCountdown(seconds: number): string {
  const total = Math.max(0, Math.ceil(seconds));
  const m = String(Math.floor(total / 60)).padStart(2, '0');
  const s = String(total % 60).padStart(2, '0');
  return `${m}:${s}`;
}
