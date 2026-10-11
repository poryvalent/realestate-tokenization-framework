import {
  memo,
  type ReactNode,
  useState,
  type ChangeEvent,
  type FormEvent,
  useEffect,
  useRef,
  forwardRef,
} from 'react';
import {
  motion,
  useAnimation,
  useInView,
  useMotionTemplate,
  useMotionValue,
} from 'motion/react';
import { Eye, EyeOff } from 'lucide-react';
import { cn } from '@/lib/utils';

// ==================== Premium Square Input Component ====================

const Input = memo(
  forwardRef(function Input(
    { className, type, ...props }: React.InputHTMLAttributes<HTMLInputElement>,
    ref: React.ForwardedRef<HTMLInputElement>
  ) {
    const radius = 120;
    const [visible, setVisible] = useState(false);

    const mouseX = useMotionValue(0);
    const mouseY = useMotionValue(0);

    function handleMouseMove({
      currentTarget,
      clientX,
      clientY,
    }: React.MouseEvent<HTMLDivElement>) {
      const { left, top } = currentTarget.getBoundingClientRect();
      mouseX.set(clientX - left);
      mouseY.set(clientY - top);
    }

    return (
      <motion.div
        style={{
          background: useMotionTemplate`
            radial-gradient(
              ${visible ? radius + 'px' : '0px'} circle at ${mouseX}px ${mouseY}px,
              rgba(255, 255, 255, 0.4),
              transparent 80%
            )
          `,
        }}
        onMouseMove={handleMouseMove}
        onMouseEnter={() => setVisible(true)}
        onMouseLeave={() => setVisible(false)}
        className="group/input rounded-xl p-[1px] transition-all duration-300"
      >
        <input
          type={type}
          className={cn(
            `flex h-11 w-full rounded-xl border border-white/15 bg-[#030816]/90 px-4 py-2.5 text-sm text-white transition-all duration-200 placeholder:text-slate-400 focus-visible:border-white focus-visible:bg-[#050c1f] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-white/20 disabled:cursor-not-allowed disabled:opacity-50`,
            className
          )}
          ref={ref}
          {...props}
        />
      </motion.div>
    );
  })
);

Input.displayName = 'Input';

// ==================== BoxReveal Component ====================

type BoxRevealProps = {
  children: ReactNode;
  width?: string;
  boxColor?: string;
  duration?: number;
  overflow?: string;
  position?: string;
  className?: string;
};

const BoxReveal = memo(function BoxReveal({
  children,
  width = 'fit-content',
  boxColor,
  duration,
  overflow = 'hidden',
  position = 'relative',
  className,
}: BoxRevealProps) {
  const mainControls = useAnimation();
  const slideControls = useAnimation();
  const ref = useRef(null);
  const isInView = useInView(ref, { once: true });

  useEffect(() => {
    if (isInView) {
      slideControls.start('visible');
      mainControls.start('visible');
    } else {
      slideControls.start('hidden');
      mainControls.start('hidden');
    }
  }, [isInView, mainControls, slideControls]);

  return (
    <section
      ref={ref}
      style={{
        position: position as
          | 'relative'
          | 'absolute'
          | 'fixed'
          | 'sticky'
          | 'static',
        width,
        overflow,
      }}
      className={className}
    >
      <motion.div
        variants={{
          hidden: { opacity: 0, y: 20 },
          visible: { opacity: 1, y: 0 },
        }}
        initial="hidden"
        animate={mainControls}
        transition={{ duration: duration ?? 0.4, delay: 0.1 }}
      >
        {children}
      </motion.div>
      <motion.div
        variants={{ hidden: { left: 0 }, visible: { left: '100%' } }}
        initial="hidden"
        animate={slideControls}
        transition={{ duration: duration ?? 0.4, ease: [0.25, 0.1, 0.25, 1] }}
        style={{
          position: 'absolute',
          top: 2,
          bottom: 2,
          left: 0,
          right: 0,
          zIndex: 20,
          background: boxColor ?? '#ffffff',
          borderRadius: 0,
        }}
      />
    </section>
  );
});

// ==================== Concentric Radial Ripple Component ====================

type RippleProps = {
  mainCircleSize?: number;
  mainCircleOpacity?: number;
  numCircles?: number;
  className?: string;
};

const Ripple = memo(function Ripple({
  mainCircleSize = 140,
  mainCircleOpacity = 0.15,
  numCircles = 5,
  className = '',
}: RippleProps) {
  return (
    <div
      className={cn(
        'pointer-events-none absolute inset-0 flex items-center justify-center overflow-hidden [mask-image:radial-gradient(ellipse_at_center,black_45%,transparent_75%)]',
        className
      )}
    >
      {Array.from({ length: numCircles }, (_, i) => {
        const size = mainCircleSize + i * 75;
        const opacity = Math.max(0.04, mainCircleOpacity - i * 0.025);
        return (
          <span
            key={i}
            className="absolute rounded-full border transition-all duration-700"
            style={{
              width: `${size}px`,
              height: `${size}px`,
              borderColor: `rgba(255, 255, 255, ${opacity})`,
              borderStyle: i % 2 === 0 ? 'solid' : 'dashed',
              borderWidth: '1px',
              top: '50%',
              left: '50%',
              transform: 'translate(-50%, -50%)',
            }}
          />
        );
      })}
    </div>
  );
});

// ==================== OrbitingCircles Component ====================

type OrbitingCirclesProps = {
  className?: string;
  children: ReactNode;
  reverse?: boolean;
  duration?: number;
  delay?: number;
  radius?: number;
  path?: boolean;
};

const OrbitingCircles = memo(function OrbitingCircles({
  className,
  children,
  reverse = false,
  duration = 24,
  delay = 0,
  radius = 60,
  path = true,
}: OrbitingCirclesProps) {
  return (
    <>
      {path && (
        <svg
          xmlns="http://www.w3.org/2000/svg"
          version="1.1"
          className="pointer-events-none absolute inset-0 size-full"
        >
          <circle
            className="stroke-white/10 stroke-1"
            strokeDasharray="3 4"
            cx="50%"
            cy="50%"
            r={radius}
            fill="none"
          />
        </svg>
      )}
      <section
        style={
          {
            animationName: 'orbit',
            animationDuration: `${duration}s`,
            animationTimingFunction: 'linear',
            animationIterationCount: 'infinite',
            animationDirection: reverse ? 'reverse' : 'normal',
            animationDelay: `${-(delay || 0)}s`,
            '--radius': radius,
          } as React.CSSProperties
        }
        className={cn(
          'absolute flex size-full transform-gpu items-center justify-center pointer-events-none',
          className
        )}
      >
        <div className="pointer-events-auto">
          {children}
        </div>
      </section>
    </>
  );
});

// ==================== Proof Orbit Display ====================

export type ProofChip = {
  label: string;
  sub: string;
  icon: React.ElementType;
  className?: string;
  duration?: number;
  delay?: number;
  radius?: number;
  reverse?: boolean;
};

export type IconConfig = {
  component: () => ReactNode;
  className?: string;
  duration?: number;
  delay?: number;
  radius?: number;
  path?: boolean;
  reverse?: boolean;
};

export type TechnologyOrbitDisplayProps = {
  chips?: ProofChip[];
  iconsArray?: IconConfig[];
  text?: string;
};

const TechOrbitDisplay = memo(function TechOrbitDisplay({
  chips,
  iconsArray,
  text,
}: TechnologyOrbitDisplayProps) {
  if (chips && chips.length > 0) {
    return (
      <section className="relative flex h-full min-h-[560px] lg:min-h-[600px] w-full flex-col items-center justify-center overflow-hidden rounded-3xl select-none">
        {/* Central Curved Luminous Attestation Core */}
        <div className="relative z-10 flex flex-col items-center justify-center text-center">
          <motion.div
            animate={{ scale: [1, 1.05, 1] }}
            transition={{ duration: 4, repeat: Infinity, ease: 'easeInOut' }}
            className="relative flex h-16 w-16 items-center justify-center rounded-3xl border border-blue-400/35 bg-gradient-to-b from-[#1e40af] via-[#091e48] to-[#040c1e] shadow-[0_0_45px_rgba(37,99,235,0.4)] backdrop-blur-2xl"
          >
            <svg viewBox="0 0 256 256" className="h-8 w-8 fill-white" aria-hidden="true">
              <path d="M228 0C172.772 0 128 44.772 128 100L128 0L0 0L0 28C0 83.228 44.772 128 100 128L0 128L0 256L28 256C83.228 256 128 211.228 128 156L128 256L256 256L256 228C256 172.772 211.228 128 156 128L256 128L256 0Z" />
            </svg>
          </motion.div>

          <h3 className="mt-3 font-sans text-base font-bold tracking-tight text-white uppercase">
            {text || 'AcreSync'}
          </h3>
          <p className="font-mono text-[9px] tracking-[0.25em] uppercase text-blue-300/80">
            Attestation Core
          </p>
        </div>

        {/* Crisp Celestial Orbit Guideways */}
        <svg
          xmlns="http://www.w3.org/2000/svg"
          version="1.1"
          className="pointer-events-none absolute inset-0 size-full"
        >
          <circle
            className="stroke-blue-400/25 stroke-[1px]"
            strokeDasharray="4 6"
            cx="50%"
            cy="50%"
            r="110"
            fill="none"
          />
          <circle
            className="stroke-white/15 stroke-[1px]"
            strokeDasharray="3 5"
            cx="50%"
            cy="50%"
            r="180"
            fill="none"
          />
        </svg>

        {/* Orbiting Curved Proof Chips */}
        {chips.map((chip, index) => {
          const Icon = chip.icon;
          return (
            <OrbitingCircles
              key={index}
              className={chip.className}
              duration={chip.duration ?? 26}
              delay={chip.delay ?? 0}
              radius={chip.radius ?? 140}
              reverse={chip.reverse ?? false}
              path={false}
            >
              <div className="group flex items-center gap-2.5 rounded-full border border-white/20 bg-[#07173b]/90 px-3.5 py-1.5 shadow-[0_8px_30px_rgba(0,0,0,0.65)] backdrop-blur-xl transition-all duration-300 hover:scale-105 hover:border-white hover:bg-[#0c245c]">
                <span className="flex h-6 w-6 items-center justify-center rounded-full bg-white/10 text-white border border-white/20 shrink-0">
                  <Icon className="h-3.5 w-3.5" />
                </span>
                <div className="flex flex-col text-left">
                  <span className="font-sans text-[11px] font-semibold text-white leading-tight whitespace-nowrap">
                    {chip.label}
                  </span>
                  {chip.sub && (
                    <span className="font-mono text-[9px] text-slate-300 leading-tight whitespace-nowrap">
                      {chip.sub}
                    </span>
                  )}
                </div>
              </div>
            </OrbitingCircles>
          );
        })}
      </section>
    );
  }

  const icons = iconsArray || [];
  return (
    <section className="relative flex h-full min-h-[460px] w-full flex-col items-center justify-center overflow-hidden rounded-none select-none">
      <span className="pointer-events-none whitespace-pre-wrap text-center text-5xl font-bold uppercase tracking-wider text-white">
        {text || 'AcreSync'}
      </span>

      {icons.map((icon, index) => (
        <OrbitingCircles
          key={index}
          className={icon.className}
          duration={icon.duration ?? 20}
          delay={icon.delay ?? 10}
          radius={icon.radius ?? 120}
          path={icon.path ?? true}
          reverse={icon.reverse ?? false}
        >
          {icon.component()}
        </OrbitingCircles>
      ))}
    </section>
  );
});

// ==================== Animated Form ====================

export type FieldType = 'text' | 'email' | 'password';

export type Field = {
  label: string;
  required?: boolean;
  type: FieldType;
  placeholder?: string;
  value?: string;
  onChange: (event: ChangeEvent<HTMLInputElement>) => void;
};

export type AnimatedFormProps = {
  header: string;
  subHeader?: string;
  fields: Field[];
  submitButton: string;
  textVariantButton?: string;
  errorField?: string;
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
  goTo?: (event: React.MouseEvent<HTMLButtonElement | HTMLAnchorElement>) => void;
  extraContent?: ReactNode;
};

type Errors = {
  [key: string]: string;
};

const AnimatedForm = memo(function AnimatedForm({
  header,
  subHeader,
  fields,
  submitButton,
  textVariantButton,
  errorField,
  onSubmit,
  goTo,
  extraContent,
}: AnimatedFormProps) {
  const [visible, setVisible] = useState<boolean>(false);
  const [errors, setErrors] = useState<Errors>({});

  const toggleVisibility = () => setVisible(!visible);

  const validateForm = (event: FormEvent<HTMLFormElement>) => {
    const currentErrors: Errors = {};
    fields.forEach((field) => {
      const inputEl = (event.target as HTMLFormElement)[field.label] as HTMLInputElement | undefined;
      const value = inputEl ? inputEl.value : field.value;

      if (field.required && !value) {
        currentErrors[field.label] = `${field.label} is required`;
      }
      if (field.type === 'email' && value && !/\S+@\S+\.\S+/.test(value)) {
        currentErrors[field.label] = 'Invalid email address';
      }
      if (field.type === 'password' && value && value.length < 6) {
        currentErrors[field.label] = 'Password must be at least 6 characters';
      }
    });
    return currentErrors;
  };

  const handleSubmit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const formErrors = validateForm(event);
    if (Object.keys(formErrors).length === 0) {
      onSubmit(event);
    } else {
      setErrors(formErrors);
    }
  };

  return (
    <div className="flex w-full flex-col gap-4">
      <BoxReveal boxColor="#ffffff" duration={0.3}>
        <h2 className="font-sans text-3xl font-bold tracking-tight text-white uppercase">
          {header}
        </h2>
      </BoxReveal>

      {subHeader && (
        <BoxReveal boxColor="#ffffff" duration={0.3} className="pb-1">
          <p className="text-sm leading-relaxed text-slate-300">
            {subHeader}
          </p>
        </BoxReveal>
      )}

      <form onSubmit={handleSubmit} className="mt-2 space-y-4">
        {fields.map((field) => (
          <div key={field.label} className="flex flex-col gap-1.5">
            <BoxReveal boxColor="#ffffff" duration={0.3}>
              <Label htmlFor={field.label}>
                {field.label} {field.required && <span className="text-rose-400">*</span>}
              </Label>
            </BoxReveal>

            <BoxReveal width="100%" boxColor="#ffffff" duration={0.3}>
              <div className="relative">
                <Input
                  type={
                    field.type === 'password'
                      ? visible
                        ? 'text'
                        : 'password'
                      : field.type
                  }
                  id={field.label}
                  placeholder={field.placeholder}
                  value={field.value}
                  onChange={field.onChange}
                />

                {field.type === 'password' && (
                  <button
                    type="button"
                    onClick={toggleVisibility}
                    className="absolute inset-y-0 right-0 flex items-center pr-3.5 text-white/50 hover:text-white"
                  >
                    {visible ? <Eye className="h-4 w-4" /> : <EyeOff className="h-4 w-4" />}
                  </button>
                )}
              </div>

              {errors[field.label] && (
                <p className="mt-1 text-xs text-rose-400">
                  {errors[field.label]}
                </p>
              )}
            </BoxReveal>
          </div>
        ))}

        {extraContent && (
          <div className="pt-1">
            {extraContent}
          </div>
        )}

        {errorField && (
          <BoxReveal width="100%" boxColor="#ffffff" duration={0.3}>
            <p className="rounded-none border border-rose-500/30 bg-rose-500/10 px-3 py-2 text-xs text-rose-300">
              {errorField}
            </p>
          </BoxReveal>
        )}

        <BoxReveal width="100%" boxColor="#ffffff" duration={0.3} overflow="visible">
          <button
            type="submit"
            className="rounded-none bg-white text-[#040916] font-bold text-xs uppercase tracking-wider h-11 w-full flex items-center justify-center gap-2 hover:bg-slate-100 transition-all cursor-pointer shadow-md"
          >
            <span>{submitButton}</span>
            <span>&rarr;</span>
          </button>
        </BoxReveal>

        {textVariantButton && goTo && (
          <BoxReveal boxColor="#ffffff" duration={0.3}>
            <div className="pt-1 text-center">
              <button
                type="button"
                className="text-xs font-semibold text-slate-300 hover:text-white underline cursor-pointer"
                onClick={goTo}
              >
                {textVariantButton}
              </button>
            </div>
          </BoxReveal>
        )}
      </form>
    </div>
  );
});

// ==================== AuthTabs Component ====================

export type AuthTabsProps = {
  formFields: Omit<AnimatedFormProps, 'onSubmit'> & { onSubmit?: (event: FormEvent<HTMLFormElement>) => void };
  goTo?: (event: React.MouseEvent<HTMLButtonElement | HTMLAnchorElement>) => void;
  handleSubmit: (event: FormEvent<HTMLFormElement>) => void;
};

const AuthTabs = memo(function AuthTabs({
  formFields,
  goTo,
  handleSubmit,
}: AuthTabsProps) {
  return (
    <div className="w-full max-w-md">
      <AnimatedForm
        header={formFields.header}
        subHeader={formFields.subHeader}
        fields={formFields.fields}
        submitButton={formFields.submitButton}
        textVariantButton={formFields.textVariantButton}
        errorField={formFields.errorField}
        onSubmit={handleSubmit}
        goTo={goTo}
        extraContent={formFields.extraContent}
      />
    </div>
  );
});

const BottomGradient = () => {
  return (
    <>
      <span className="absolute inset-x-0 -bottom-px h-px w-full bg-gradient-to-r from-transparent via-white to-transparent opacity-0 transition duration-500 group-hover:opacity-100" />
    </>
  );
};

// ==================== Label Component ====================

interface LabelProps extends React.LabelHTMLAttributes<HTMLLabelElement> {
  htmlFor?: string;
}

const Label = memo(function Label({ className, ...props }: LabelProps) {
  return (
    <label
      className={cn(
        'text-xs font-bold uppercase tracking-wider text-slate-300',
        className
      )}
      {...props}
    />
  );
});

export {
  Input,
  BoxReveal,
  Ripple,
  OrbitingCircles,
  TechOrbitDisplay,
  AnimatedForm,
  AuthTabs,
  Label,
  BottomGradient,
};
