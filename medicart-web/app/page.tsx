"use client";

import { useEffect, useState } from "react";

type RecordEntry = {
  timestamp: string;
  patient_name?: string;
  clinic_name?: string;
  [k: string]: any;
};

export default function Home() {
  const [doctor, setDoctor] = useState("");
  const [clinics, setClinics] = useState<string[]>([]);
  const [clinic, setClinic] = useState("");
  const [patients, setPatients] = useState<string[]>([]);
  const [patient, setPatient] = useState("");
  const [data, setData] = useState<Record<string, RecordEntry[]>>({});
  const [camSrc, setCamSrc] = useState("");
  const [camStatus, setCamStatus] = useState("Requires camera feed");
  const [ws, setWs] = useState<WebSocket | null>(null);
  const [loadingPatients, setLoadingPatients] = useState(false);
  const [loadingData, setLoadingData] = useState(false);

  const API_BASE = process.env.NEXT_PUBLIC_API_BASE ?? "http://localhost:8081";

  useEffect(() => {
    loadClinics();
  }, []);

  useEffect(() => {
    if (!clinic) return;
    loadPatients(clinic);
  }, [clinic]);

  useEffect(() => {
    if (clinic && patient) {
      loadPatientData(clinic, patient);
      connectStream(clinic, patient);
    } else {
      setData({});
      setCamSrc("");
      disconnectStream();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [clinic, patient]);

  async function loadClinics() {
    try {
      const base = (API_BASE || "").replace(/\/$/, "");
      const res = await fetch(`${base}/api/clinics`);
      const json = await res.json();
      setClinics(Array.isArray(json) ? json : []);
    } catch {
      setClinics([]);
    }
  }

  async function loadPatients(c: string) {
    setLoadingPatients(true);
    try {
      const res = await fetch(`${API_BASE}/api/clinic/${c}/patients`);
      const json = await res.json();
      setPatients(Array.isArray(json) ? json : []);
    } catch {
      setPatients([]);
    } finally {
      setLoadingPatients(false);
    }
  }

  async function loadPatientData(c: string, p: string) {
    setLoadingData(true);
    try {
      const res = await fetch(`${API_BASE}/api/clinic/${c}/patient/${p}/data`);
      const json = await res.json();
      setData(json || {});
    } catch {
      setData({});
    } finally {
      setLoadingData(false);
    }
  }

  function connectStream(c: string, p: string) {
    disconnectStream();
    const base = (API_BASE || "").replace(/\/$/, "");
    const httpUrl = `${base}/ws/stream?clinic=${encodeURIComponent(c)}&patient=${encodeURIComponent(p)}`;
    const wsUrl = httpUrl.replace(/^http/, "ws");
    const sock = new WebSocket(wsUrl);
    sock.binaryType = "arraybuffer";
    sock.onopen = () => setCamStatus("Connected to stream");
    sock.onclose = () => {
      setCamStatus("Stream disconnected");
      setWs(null);
    };
    sock.onerror = () => setCamStatus("Stream error");
    sock.onmessage = (ev) => {
      if (ev.data instanceof ArrayBuffer) {
        const blob = new Blob([ev.data], { type: "image/jpeg" });
        const urlObj = URL.createObjectURL(blob);
        setCamSrc(urlObj);
        setCamStatus("Streaming");
      }
    };
    setWs(sock);
  }

  function disconnectStream() {
    if (ws) {
      ws.close();
      setWs(null);
    }
  }

  return (
    <div className="min-h-screen bg-slate-50 text-slate-900">
      <div className="max-w-6xl mx-auto px-4 py-8 space-y-6">
        <header className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <h1 className="text-2xl font-semibold">Medicart Web</h1>
            <p className="text-sm text-slate-600">
              Clinic dashboard for patients and camera feed
            </p>
          </div>
          <div className="flex items-center gap-3">
            <input
              value={doctor}
              onChange={(e) => setDoctor(e.target.value)}
              placeholder="Doctor name"
              className="px-3 py-2 rounded border border-slate-200 text-sm focus:outline-none focus:ring focus:ring-indigo-200 bg-white"
            />
            <button className="px-4 py-2 rounded bg-indigo-600 text-white text-sm font-medium hover:bg-indigo-500">
              Log In
            </button>
          </div>
        </header>

        <section className="grid grid-cols-1 md:grid-cols-3 gap-4">
          <div className="md:col-span-1 space-y-3">
            <div className="p-4 bg-white rounded shadow-sm border border-slate-100">
              <h2 className="text-sm font-semibold text-slate-700 mb-2">Clinics</h2>
              <select
                value={clinic}
                onChange={(e) => setClinic(e.target.value)}
                className="w-full px-3 py-2 rounded border border-slate-200 text-sm focus:outline-none focus:ring focus:ring-indigo-200 bg-white"
              >
                <option value="">Select clinic</option>
                {clinics.map((c) => (
                  <option key={c} value={c}>
                    {c}
                  </option>
                ))}
              </select>
              <div className="mt-2">
                <button
                  onClick={loadClinics}
                  className="px-3 py-2 rounded bg-slate-800 text-white text-xs font-medium hover:bg-slate-700"
                >
                  Refresh Clinics
                </button>
              </div>
            </div>

            <div className="p-4 bg-white rounded shadow-sm border border-slate-100">
              <h2 className="text-sm font-semibold text-slate-700 mb-2">Patients</h2>
              {loadingPatients ? (
                <div className="text-slate-400 text-sm">Loading...</div>
              ) : patients.length ? (
                <ul className="space-y-2 text-sm text-slate-700">
                  {patients.map((p) => (
                    <li key={p}>
                      <button
                        className="text-indigo-600 hover:underline"
                        onClick={() => setPatient(p)}
                      >
                        {p}
                      </button>
                    </li>
                  ))}
                </ul>
              ) : (
                <div className="text-slate-400 text-sm">
                  {clinic ? "No patients" : "Select a clinic"}
                </div>
              )}
            </div>
          </div>

          <div className="md:col-span-2 space-y-4">
            <div className="p-4 bg-white rounded shadow-sm border border-slate-100">
              <div className="flex items-center justify-between">
                <div>
                  <h2 className="text-sm font-semibold text-slate-700">Patient Data</h2>
                  <p className="text-xs text-slate-500">
                    {patient ? `Clinic: ${clinic} • Patient: ${patient}` : "Choose a patient"}
                  </p>
                </div>
                <button
                  onClick={() => patient && loadPatientData(clinic, patient)}
                  className="px-3 py-2 rounded bg-indigo-600 text-white text-xs font-medium hover:bg-indigo-500 disabled:opacity-50"
                  disabled={!patient}
                >
                  Refresh
                </button>
              </div>
              <div className="mt-3 text-sm text-slate-800 space-y-2">
                {loadingData ? (
                  <div className="text-slate-400 text-sm">Loading...</div>
                ) : Object.keys(data).length === 0 ? (
                  <div className="text-slate-400 text-sm">No data loaded.</div>
                ) : (
                  Object.entries(data).map(([file, recs]) => (
                    <div key={file} className="p-2 border border-slate-200 rounded">
                      <div className="text-xs font-semibold text-slate-600">{file}</div>
                      <pre className="text-xs text-slate-800 whitespace-pre-wrap">
                        {JSON.stringify(recs, null, 2)}
                      </pre>
                    </div>
                  ))
                )}
              </div>
            </div>

            <div className="p-4 bg-white rounded shadow-sm border border-slate-100">
              <div className="flex items-center justify-between">
                <div>
                  <h2 className="text-sm font-semibold text-slate-700">Camera View</h2>
                  <p className="text-xs text-slate-500">{camStatus}</p>
                </div>
                <div className="flex gap-2">
                  <button
                    onClick={() => clinic && patient && connectStream(clinic, patient)}
                    className="px-3 py-2 rounded bg-slate-800 text-white text-xs font-medium hover:bg-slate-700 disabled:opacity-50"
                    disabled={!patient}
                  >
                    Connect Stream
                  </button>
                  <button
                    onClick={disconnectStream}
                    className="px-3 py-2 rounded bg-slate-200 text-slate-900 text-xs font-medium hover:bg-slate-300 disabled:opacity-50"
                    disabled={!ws}
                  >
                    Disconnect
                  </button>
                </div>
              </div>
              <div className="mt-3 border border-slate-200 rounded overflow-hidden bg-slate-100 min-h-[200px] flex items-center justify-center">
                {camSrc ? (
                  <img
                    src={camSrc}
                    alt="Camera"
                    className="w-full max-h-[360px] object-contain"
                  />
                ) : (
                  <div className="text-slate-400 text-sm">No camera image available</div>
                )}
              </div>
            </div>
          </div>
        </section>
      </div>
    </div>
  );
}
