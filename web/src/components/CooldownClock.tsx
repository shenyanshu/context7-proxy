import { Tooltip } from 'antd';
import dayjs from 'dayjs';
import { useEffect, useState } from 'react';
import { formatCountdown } from '../format';

interface CooldownClockProps {
  /** 冷却结束时间（Unix 秒）；不在冷却中的 key 不应渲染本组件 */
  cooldownUntil: number;
}

/** 冷却倒计时，数字等宽展示，鼠标悬停可见具体解冻时刻 */
export function CooldownClock({ cooldownUntil }: CooldownClockProps) {
  const [remainMs, setRemainMs] = useState(() => cooldownUntil * 1000 - Date.now());

  useEffect(() => {
    // 每秒自tick重新计算，不依赖父组件轮询节奏，保证倒计时平滑
    const timer = window.setInterval(() => {
      setRemainMs(cooldownUntil * 1000 - Date.now());
    }, 1000);
    return () => window.clearInterval(timer);
  }, [cooldownUntil]);

  const remainSec = Math.max(0, Math.round(remainMs / 1000));
  const endTime = dayjs.unix(cooldownUntil).format('HH:mm:ss');

  return (
    <Tooltip title={`将于 ${endTime} 解冻`}>
      <span className="stat-num stat-num--warn">{formatCountdown(remainSec)}</span>
    </Tooltip>
  );
}
