/**
 * Момент чтения данных (#268): от него список роёв и обзор считают, сколько
 * прошло с момента исполнения. Своего таймера нет: момент сдвигается, когда
 * приходят новые данные -- с опросом обзора (#257) и с перечитыванием списка.
 * Layout-эффект: строка с новыми данными и прежним моментом не рисуется.
 */
import { useLayoutEffect, useState } from 'react';

export function useReadMoment(data: unknown): number {
  const [moment, setMoment] = useState(Date.now);
  useLayoutEffect(() => {
    setMoment(Date.now());
  }, [data]);
  return moment;
}
