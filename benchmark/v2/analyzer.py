#!/usr/bin/env python3
"""
Script d'analyse des résultats de benchmark QUIC
Génère des graphiques et des statistiques détaillées
"""

import pandas as pd
import numpy as np
import matplotlib.pyplot as plt
import seaborn as sns
import json
from datetime import datetime
import argparse
import os
from scipy import stats
from matplotlib.backends.backend_pdf import PdfPages
import warnings
warnings.filterwarnings('ignore')

# Configuration du style des graphiques
plt.style.use('seaborn-v0_8')
sns.set_palette("husl")

class QUICBenchmarkAnalyzer:
    def __init__(self, csv_file='benchmark_results.csv', json_file='benchmark_results.json'):
        """Initialise l'analyseur avec les fichiers de résultats"""
        self.csv_file = csv_file
        self.json_file = json_file
        self.df = None
        self.json_data = None
        self.load_data()
        
    def load_data(self):
        """Charge les données depuis les fichiers CSV et JSON"""
        try:
            # Chargement du CSV
            if os.path.exists(self.csv_file):
                self.df = pd.read_csv(self.csv_file)
                self.df['timestamp'] = pd.to_datetime(self.df['timestamp'])
                print(f"✅ Données CSV chargées: {len(self.df)} tests")
            else:
                print(f"❌ Fichier CSV non trouvé: {self.csv_file}")
                
            # Chargement du JSON pour les détails
            if os.path.exists(self.json_file):
                with open(self.json_file, 'r') as f:
                    self.json_data = json.load(f)
                print(f"✅ Données JSON chargées: {len(self.json_data)} tests")
            else:
                print(f"❌ Fichier JSON non trouvé: {self.json_file}")
                
        except Exception as e:
            print(f"❌ Erreur chargement des données: {e}")
            return False
            
    def generate_summary_statistics(self):
        """Génère les statistiques de résumé"""
        if self.df is None:
            return
            
        print("\n" + "="*60)
        print("📊 STATISTIQUES DE RÉSUMÉ DU BENCHMARK QUIC")
        print("="*60)
        
        # Statistiques générales
        print(f"\n🔍 Vue d'ensemble:")
        print(f"  • Nombre total de tests: {len(self.df)}")
        print(f"  • Période de test: {self.df['timestamp'].min()} - {self.df['timestamp'].max()}")
        print(f"  • Scénarios testés: {', '.join(self.df['test_name'].unique())}")
        
        # Statistiques de performance
        print(f"\n⚡ Performance globale:")
        print(f"  • RPS moyen: {self.df['requests_per_second'].mean():.2f}")
        print(f"  • RPS maximum: {self.df['requests_per_second'].max():.2f}")
        print(f"  • Latence moyenne: {self.df['avg_latency_ms'].mean():.2f} ms")
        print(f"  • Latence P95 moyenne: {self.df['p95_latency_ms'].mean():.2f} ms")
        print(f"  • Débit moyen: {self.df['throughput_mbps'].mean():.2f} Mbps")
        
        # Statistiques par scénario
        print(f"\n📈 Performance par scénario:")
        scenario_stats = self.df.groupby('test_name').agg({
            'requests_per_second': ['mean', 'max'],
            'avg_latency_ms': ['mean', 'min'],
            'throughput_mbps': ['mean', 'max'],
            'successful_requests': 'sum',
            'failed_requests': 'sum'
        }).round(2)
        
        for scenario in self.df['test_name'].unique():
            scenario_data = self.df[self.df['test_name'] == scenario]
            success_rate = (scenario_data['successful_requests'].sum() / 
                          (scenario_data['successful_requests'].sum() + scenario_data['failed_requests'].sum()) * 100)
            print(f"  • {scenario}:")
            print(f"    - RPS: {scenario_data['requests_per_second'].mean():.2f}")
            print(f"    - Latence: {scenario_data['avg_latency_ms'].mean():.2f} ms")
            print(f"    - Débit: {scenario_data['throughput_mbps'].mean():.2f} Mbps")
            print(f"    - Taux de succès: {success_rate:.1f}%")
        
        # Corrélations
        print(f"\n🔗 Corrélations importantes:")
        corr_matrix = self.df[['concurrent_clients', 'message_size', 'requests_per_second', 
                              'avg_latency_ms', 'throughput_mbps', 'packet_loss']].corr()
        
        print(f"  • Concurrence vs RPS: {corr_matrix.loc['concurrent_clients', 'requests_per_second']:.3f}")
        print(f"  • Taille message vs Latence: {corr_matrix.loc['message_size', 'avg_latency_ms']:.3f}")
        print(f"  • Perte paquets vs RPS: {corr_matrix.loc['packet_loss', 'requests_per_second']:.3f}")

    def plot_performance_overview(self):
        """Génère les graphiques de vue d'ensemble des performances"""
        if self.df is None:
            return
            
        fig, axes = plt.subplots(2, 2, figsize=(16, 12))
        fig.suptitle('📊 Vue d\'ensemble des performances QUIC', fontsize=16, fontweight='bold')
        
        # 1. RPS par scénario
        ax1 = axes[0, 0]
        scenario_rps = self.df.groupby('test_name')['requests_per_second'].mean().sort_values(ascending=False)
        bars1 = ax1.bar(range(len(scenario_rps)), scenario_rps.values, color=plt.cm.viridis(np.linspace(0, 1, len(scenario_rps))))
        ax1.set_title('Requêtes par seconde par scénario', fontweight='bold')
        ax1.set_ylabel('RPS')
        ax1.set_xticks(range(len(scenario_rps)))
        ax1.set_xticklabels(scenario_rps.index, rotation=45, ha='right')
        
        # Ajout des valeurs sur les barres
        for i, bar in enumerate(bars1):
            height = bar.get_height()
            ax1.text(bar.get_x() + bar.get_width()/2., height,
                    f'{height:.0f}', ha='center', va='bottom')
        
        # 2. Distribution des latences
        ax2 = axes[0, 1]
        latency_data = [self.df[self.df['test_name'] == scenario]['avg_latency_ms'].values 
                       for scenario in self.df['test_name'].unique()]
        ax2.boxplot(latency_data, labels=self.df['test_name'].unique())
        ax2.set_title('Distribution des latences moyennes', fontweight='bold')
        ax2.set_ylabel('Latence (ms)')
        ax2.tick_params(axis='x', rotation=45)
        
        # 3. Débit par taille de message
        ax3 = axes[1, 0]
        scatter = ax3.scatter(self.df['message_size'], self.df['throughput_mbps'], 
                            c=self.df['concurrent_clients'], cmap='viridis', alpha=0.6, s=50)
        ax3.set_title('Débit en fonction de la taille des messages', fontweight='bold')
        ax3.set_xlabel('Taille du message (bytes)')
        ax3.set_ylabel('Débit (Mbps)')
        ax3.set_xscale('log')
        plt.colorbar(scatter, ax=ax3, label='Clients concurrents')
        
        # 4. Impact de la concurrence sur les performances
        ax4 = axes[1, 1]
        concurrency_groups = self.df.groupby('concurrent_clients').agg({
            'requests_per_second': 'mean',
            'avg_latency_ms': 'mean'
        }).reset_index()
        
        ax4_twin = ax4.twinx()
        line1 = ax4.plot(concurrency_groups['concurrent_clients'], concurrency_groups['requests_per_second'], 
                        'b-o', label='RPS', linewidth=2)
        line2 = ax4_twin.plot(concurrency_groups['concurrent_clients'], concurrency_groups['avg_latency_ms'], 
                             'r-s', label='Latence', linewidth=2)
        
        ax4.set_title('Impact de la concurrence', fontweight='bold')
        ax4.set_xlabel('Clients concurrents')
        ax4.set_ylabel('RPS', color='b')
        ax4_twin.set_ylabel('Latence moyenne (ms)', color='r')
        ax4.tick_params(axis='y', labelcolor='b')
        ax4_twin.tick_params(axis='y', labelcolor='r')
        
        # Légende combinée
        lines1, labels1 = ax4.get_legend_handles_labels()
        lines2, labels2 = ax4_twin.get_legend_handles_labels()
        ax4.legend(lines1 + lines2, labels1 + labels2, loc='upper left')
        
        plt.tight_layout()
        plt.savefig('quic_performance_overview.png', dpi=300, bbox_inches='tight')
        plt.show()

    def plot_latency_analysis(self):
        """Analyse détaillée des latences"""
        if self.df is None:
            return
            
        fig, axes = plt.subplots(2, 2, figsize=(16, 12))
        fig.suptitle('🕐 Analyse détaillée des latences', fontsize=16, fontweight='bold')
        
        # 1. Percentiles de latence par scénario
        ax1 = axes[0, 0]
        scenarios = self.df['test_name'].unique()
        x_pos = np.arange(len(scenarios))
        width = 0.2
        
        for i, percentile in enumerate(['avg_latency_ms', 'median_latency_ms', 'p95_latency_ms', 'p99_latency_ms']):
            values = [self.df[self.df['test_name'] == scenario][percentile].mean() for scenario in scenarios]
            ax1.bar(x_pos + i * width, values, width, 
                   label=percentile.replace('_latency_ms', '').upper())
        
        ax1.set_title('Percentiles de latence par scénario')
        ax1.set_xlabel('Scénarios')
        ax1.set_ylabel('Latence (ms)')
        ax1.set_xticks(x_pos + width * 1.5)
        ax1.set_xticklabels(scenarios, rotation=45, ha='right')
        ax1.legend()
        ax1.set_yscale('log')
        
        # 2. Heatmap latence vs conditions réseau
        ax2 = axes[0, 1]
        pivot_data = self.df.pivot_table(values='avg_latency_ms', 
                                        index='network_latency_ms', 
                                        columns='packet_loss', 
                                        aggfunc='mean')
        sns.heatmap(pivot_data, annot=True, fmt='.1f', ax=ax2, cmap='YlOrRd')
        ax2.set_title('Latence selon conditions réseau')
        ax2.set_xlabel('Perte de paquets (%)')
        ax2.set_ylabel('Latence réseau (ms)')
        
        # 3. Évolution temporelle des latences
        ax3 = axes[1, 0]
        for scenario in self.df['test_name'].unique():
            scenario_data = self.df[self.df['test_name'] == scenario].sort_values('timestamp')
            ax3.plot(scenario_data['timestamp'], scenario_data['avg_latency_ms'], 
                    marker='o', label=scenario, linewidth=2)
        
        ax3.set_title('Évolution temporelle des latences')
        ax3.set_xlabel('Timestamp')
        ax3.set_ylabel('Latence moyenne (ms)')
        ax3.legend(bbox_to_anchor=(1.05, 1), loc='upper left')
        ax3.tick_params(axis='x', rotation=45)
        
        # 4. Distribution des latences (violin plot)
        ax4 = axes[1, 1]
        latency_columns = ['min_latency_ms', 'median_latency_ms', 'avg_latency_ms', 'p95_latency_ms', 'p99_latency_ms', 'max_latency_ms']
        latency_data = []
        labels = []
        
        for col in latency_columns:
            latency_data.append(self.df[col].values)
            labels.append(col.replace('_latency_ms', '').upper())
        
        parts = ax4.violinplot(latency_data, positions=range(len(labels)), showmeans=True)
        ax4.set_title('Distribution des métriques de latence')
        ax4.set_xlabel('Métriques')
        ax4.set_ylabel('Latence (ms)')
        ax4.set_xticks(range(len(labels)))
        ax4.set_xticklabels(labels, rotation=45)
        ax4.set_yscale('log')
        
        plt.tight_layout()
        plt.savefig('quic_latency_analysis.png', dpi=300, bbox_inches='tight')
        plt.show()

    def plot_throughput_analysis(self):
        """Analyse du débit et de la bande passante"""
        if self.df is None:
            return
            
        fig, axes = plt.subplots(2, 2, figsize=(16, 12))
        fig.suptitle('🚀 Analyse du débit et de la bande passante', fontsize=16, fontweight='bold')
        
        # 1. Débit par scénario
        ax1 = axes[0, 0]
        scenario_throughput = self.df.groupby('test_name')['throughput_mbps'].mean().sort_values(ascending=False)
        bars = ax1.bar(range(len(scenario_throughput)), scenario_throughput.values, 
                      color=plt.cm.plasma(np.linspace(0, 1, len(scenario_throughput))))
        ax1.set_title('Débit moyen par scénario')
        ax1.set_ylabel('Débit (Mbps)')
        ax1.set_xticks(range(len(scenario_throughput)))
        ax1.set_xticklabels(scenario_throughput.index, rotation=45, ha='right')
        
        for i, bar in enumerate(bars):
            height = bar.get_height()
            ax1.text(bar.get_x() + bar.get_width()/2., height,
                    f'{height:.1f}', ha='center', va='bottom')
        
        # 2. Corrélation débit vs RPS
        ax2 = axes[0, 1]
        scatter = ax2.scatter(self.df['requests_per_second'], self.df['throughput_mbps'], 
                            c=self.df['message_size'], cmap='viridis', alpha=0.6, s=50)
        ax2.set_title('Corrélation Débit vs RPS')
        ax2.set_xlabel('Requêtes par seconde')
        ax2.set_ylabel('Débit (Mbps)')
        plt.colorbar(scatter, ax=ax2, label='Taille message (bytes)')
        
        # Ligne de tendance
        z = np.polyfit(self.df['requests_per_second'], self.df['throughput_mbps'], 1)
        p = np.poly1d(z)
        ax2.plot(self.df['requests_per_second'], p(self.df['requests_per_second']), 
                "r--", alpha=0.8, linewidth=2)
        
        # 3. Efficacité par taille de message
        ax3 = axes[1, 0]
        size_efficiency = self.df.groupby('message_size').agg({
            'throughput_mbps': 'mean',
            'requests_per_second': 'mean'
        }).reset_index()
        
        ax3.plot(size_efficiency['message_size'], size_efficiency['throughput_mbps'], 
                'bo-', linewidth=2, markersize=8, label='Débit')
        ax3.set_title('Efficacité par taille de message')
        ax3.set_xlabel('Taille du message (bytes)')
        ax3.set_ylabel('Débit (Mbps)')
        ax3.set_xscale('log')
        ax3.grid(True, alpha=0.3)
        
        # 4. Impact des conditions réseau sur le débit
        ax4 = axes[1, 1]
        
        # Créer des groupes basés sur la qualité du réseau
        self.df['network_quality'] = pd.cut(self.df['packet_loss'], 
                                          bins=[-0.1, 0.1, 1.0, float('inf')], 
                                          labels=['Bon', 'Moyen', 'Mauvais'])
        
        network_throughput = self.df.groupby(['network_quality', 'bandwidth_mbps'])['throughput_mbps'].mean().unstack()
        network_throughput.plot(kind='bar', ax=ax4, width=0.8)
        ax4.set_title('Impact des conditions réseau')
        ax4.set_xlabel('Qualité du réseau')
        ax4.set_ylabel('Débit (Mbps)')
        ax4.legend(title='Bande passante (Mbps)', bbox_to_anchor=(1.05, 1), loc='upper left')
        ax4.set_xticklabels(ax4.get_xticklabels(), rotation=0)
        
        plt.tight_layout()
        plt.savefig('quic_throughput_analysis.png', dpi=300, bbox_inches='tight')
        plt.show()

    def plot_scalability_analysis(self):
        """Analyse de la scalabilité et de la montée en charge"""
        if self.df is None:
            return
            
        fig, axes = plt.subplots(2, 2, figsize=(16, 12))
        fig.suptitle('📈 Analyse de scalabilité', fontsize=16, fontweight='bold')
        
        # 1. Performance vs nombre de clients concurrents
        ax1 = axes[0, 0]
        concurrency_perf = self.df.groupby('concurrent_clients').agg({
            'requests_per_second': ['mean', 'std'],
            'avg_latency_ms': ['mean', 'std']
        }).round(2)
        
        x = concurrency_perf.index
        y_rps = concurrency_perf[('requests_per_second', 'mean')]
        y_rps_std = concurrency_perf[('requests_per_second', 'std')]
        
        ax1.errorbar(x, y_rps, yerr=y_rps_std, marker='o', linewidth=2, capsize=5)
        ax1.set_title('RPS vs Concurrence')
        ax1.set_xlabel('Clients concurrents')
        ax1.set_ylabel('RPS')
        ax1.grid(True, alpha=0.3)
        
        # 2. Latence vs nombre de clients concurrents
        ax2 = axes[0, 1]
        y_lat = concurrency_perf[('avg_latency_ms', 'mean')]
        y_lat_std = concurrency_perf[('avg_latency_ms', 'std')]
        
        ax2.errorbar(x, y_lat, yerr=y_lat_std, marker='s', color='red', linewidth=2, capsize=5)
        ax2.set_title('Latence vs Concurrence')
        ax2.set_xlabel('Clients concurrents')
        ax2.set_ylabel('Latence moyenne (ms)')
        ax2.grid(True, alpha=0.3)
        
        # 3. Utilisation des ressources
        ax3 = axes[1, 0]
        resource_usage = self.df.groupby('concurrent_clients').agg({
            'cpu_usage': 'mean',
            'memory_usage_mb': 'mean'
        })
        
        ax3_twin = ax3.twinx()
        line1 = ax3.plot(resource_usage.index, resource_usage['cpu_usage'], 
                        'g-o', label='CPU %', linewidth=2)
        line2 = ax3_twin.plot(resource_usage.index, resource_usage['memory_usage_mb'], 
                             'orange', marker='s', label='Mémoire (MB)', linewidth=2)
        
        ax3.set_title('Utilisation des ressources')
        ax3.set_xlabel('Clients concurrents')
        ax3.set_ylabel('CPU (%)', color='g')
        ax3_twin.set_ylabel('Mémoire (MB)', color='orange')
        ax3.tick_params(axis='y', labelcolor='g')
        ax3_twin.tick_params(axis='y', labelcolor='orange')
        
        # Légende combinée
        lines1, labels1 = ax3.get_legend_handles_labels()
        lines2, labels2 = ax3_twin.get_legend_handles_labels()
        ax3.legend(lines1 + lines2, labels1 + labels2, loc='upper left')
        
        # 4. Taux de succès vs charge
        ax4 = axes[1, 1]
        success_rate = self.df.groupby('concurrent_clients').apply(
            lambda x: x['successful_requests'].sum() / (x['successful_requests'].sum() + x['failed_requests'].sum()) * 100
        )
        
        ax4.plot(success_rate.index, success_rate.values, 'mo-', linewidth=3, markersize=8)
        ax4.set_title('Taux de succès vs Charge')
        ax4.set_xlabel('Clients concurrents')
        ax4.set_ylabel('Taux de succès (%)')
        ax4.set_ylim([90, 101])
        ax4.grid(True, alpha=0.3)
        
        # Ajout de seuils
        ax4.axhline(y=95, color='orange', linestyle='--', label='Seuil 95%')
        ax4.axhline(y=99, color='green', linestyle='--', label='Seuil 99%')
        ax4.legend()
        
        plt.tight_layout()
        plt.savefig('quic_scalability_analysis.png', dpi=300, bbox_inches='tight')
        plt.show()

    def plot_network_conditions_impact(self):
        """Analyse de l'impact des conditions réseau"""
        if self.df is None:
            return
            
        fig, axes = plt.subplots(2, 2, figsize=(16, 12))
        fig.suptitle('🌐 Impact des conditions réseau', fontsize=16, fontweight='bold')
        
        # 1. Performance vs latence réseau
        ax1 = axes[0, 0]
        latency_impact = self.df.groupby('network_latency_ms').agg({
            'requests_per_second': 'mean',
            'avg_latency_ms': 'mean',
            'throughput_mbps': 'mean'
        })
        
        ax1_twin = ax1.twinx()
        line1 = ax1.plot(latency_impact.index, latency_impact['requests_per_second'], 
                        'b-o', label='RPS', linewidth=2)
        line2 = ax1_twin.plot(latency_impact.index, latency_impact['avg_latency_ms'], 
                             'r-s', label='Latence app', linewidth=2)
        
        ax1.set_title('Impact de la latence réseau')
        ax1.set_xlabel('Latence réseau (ms)')
        ax1.set_ylabel('RPS', color='b')
        ax1_twin.set_ylabel('Latence application (ms)', color='r')
        ax1.tick_params(axis='y', labelcolor='b')
        ax1_twin.tick_params(axis='y', labelcolor='r')
        
        lines1, labels1 = ax1.get_legend_handles_labels()
        lines2, labels2 = ax1_twin.get_legend_handles_labels()
        ax1.legend(lines1 + lines2, labels1 + labels2, loc='upper right')
        
        # 2. Performance vs perte de paquets
        ax2 = axes[0, 1]
        packet_loss_impact = self.df.groupby('packet_loss').agg({
            'requests_per_second': 'mean',
            'successful_requests': 'mean',
            'failed_requests': 'mean'
        })
        
        ax2.plot(packet_loss_impact.index, packet_loss_impact['requests_per_second'], 
                'go-', linewidth=2, label='RPS')
        ax2.set_title('Impact de la perte de paquets')
        ax2.set_xlabel('Perte de paquets (%)')
        ax2.set_ylabel('RPS')
        ax2.grid(True, alpha=0.3)
        
        # 3. Heatmap conditions réseau vs performance
        ax3 = axes[1, 0]
        heatmap_data = self.df.pivot_table(values='requests_per_second',
                                         index='network_latency_ms',
                                         columns='bandwidth_mbps',
                                         aggfunc='mean')
        sns.heatmap(heatmap_data, annot=True, fmt='.0f', ax=ax3, cmap='RdYlGn')
        ax3.set_title('RPS selon latence et bande passante')
        ax3.set_xlabel('Bande passante (Mbps)')
        ax3.set_ylabel('Latence réseau (ms)')
        
        # 4. Analyse de la résilience
        ax4 = axes[1, 1]
        
        # Créer des catégories de conditions réseau
        conditions = []
        for _, row in self.df.iterrows():
            if row['packet_loss'] == 0 and row['network_latency_ms'] <= 20:
                conditions.append('Excellentes')
            elif row['packet_loss'] <= 0.5 and row['network_latency_ms'] <= 50:
                conditions.append('Bonnes')
            elif row['packet_loss'] <= 1.0 and row['network_latency_ms'] <= 100:
                conditions.append('Moyennes')
            else:
                conditions.append('Difficiles')
        
        self.df['network_conditions'] = conditions
        
        resilience_data = self.df.groupby('network_conditions').agg({
            'requests_per_second': 'mean',
            'successful_requests': 'sum',
            'failed_requests': 'sum'
        })
        
        # Calcul du taux de succès
        resilience_data['success_rate'] = (resilience_data['successful_requests'] / 
                                         (resilience_data['successful_requests'] + resilience_data['failed_requests']) * 100)
        
        categories = ['Excellentes', 'Bonnes', 'Moyennes', 'Difficiles']
        success_rates = [resilience_data.loc[cat, 'success_rate'] if cat in resilience_data.index else 0 for cat in categories]
        
        bars = ax4.bar(categories, success_rates, color=['green', 'yellow', 'orange', 'red'])
        ax4.set_title('Résilience selon conditions réseau')
        ax4.set_xlabel('Conditions réseau')
        ax4.set_ylabel('Taux de succès (%)')
        ax4.set_ylim([0, 105])
        
        for i, bar in enumerate(bars):
            height = bar.get_height()
            ax4.text(bar.get_x() + bar.get_width()/2., height,
                    f'{height:.1f}%', ha='center', va='bottom')
        
        plt.tight_layout()
        plt.savefig('quic_network_conditions_analysis.png', dpi=300, bbox_inches='tight')
        plt.show()

    def generate_comparative_analysis(self):
        """Génère une analyse comparative entre les scénarios"""
        if self.df is None:
            return
            
        print("\n" + "="*60)
        print("🔍 ANALYSE COMPARATIVE DES SCÉNARIOS")
        print("="*60)
        
        # Matrice de comparaison
        scenarios = self.df['test_name'].unique()
        metrics = ['requests_per_second', 'avg_latency_ms', 'throughput_mbps', 'cpu_usage']
        
        comparison_matrix = pd.DataFrame(index=scenarios, columns=metrics)
        
        for scenario in scenarios:
            scenario_data = self.df[self.df['test_name'] == scenario]
            for metric in metrics:
                comparison_matrix.loc[scenario, metric] = scenario_data[metric].mean()
        
        print("\n📊 Matrice de performance (moyennes):")
        print(comparison_matrix.round(2))
        
        # Calcul des scores normalisés
        print("\n🏆 Scores normalisés (0-100):")
        normalized_scores = pd.DataFrame(index=scenarios, columns=metrics)
        
        for metric in metrics:
            values = comparison_matrix[metric].astype(float)
            if metric == 'avg_latency_ms':  # Plus bas est mieux
                normalized_scores[metric] = 100 * (values.max() - values) / (values.max() - values.min())
            else:  # Plus haut est mieux
                normalized_scores[metric] = 100 * (values - values.min()) / (values.max() - values.min())
        
        # Score global
        normalized_scores['score_global'] = normalized_scores.mean(axis=1)
        normalized_scores_sorted = normalized_scores.sort_values('score_global', ascending=False)
        
        print(normalized_scores_sorted.round(1))
        
        # Recommandations
        print("\n💡 RECOMMANDATIONS:")
        best_scenario = normalized_scores_sorted.index[0]
        worst_scenario = normalized_scores_sorted.index[-1]
        
        print(f"✅ Meilleur scénario global: {best_scenario}")
        print(f"❌ Scénario le moins performant: {worst_scenario}")
        
        # Recommandations par métrique
        print(f"\n📈 Recommandations par métrique:")
        for metric in metrics:
            best_for_metric = comparison_matrix[metric].astype(float).idxmax() if metric != 'avg_latency_ms' else comparison_matrix[metric].astype(float).idxmin()
            print(f"  • Meilleur pour {metric.replace('_', ' ')}: {best_for_metric}")

    def generate_detailed_report(self):
        """Génère un rapport détaillé en PDF"""
        if self.df is None:
            return
            
        with PdfPages('quic_benchmark_report.pdf') as pdf:
            # Page 1: Vue d'ensemble
            self.plot_performance_overview()
            pdf.savefig(plt.gcf(), bbox_inches='tight')
            plt.close()
            
            # Page 2: Analyse des latences
            self.plot_latency_analysis()
            pdf.savefig(plt.gcf(), bbox_inches='tight')
            plt.close()
            
            # Page 3: Analyse du débit
            self.plot_throughput_analysis()
            pdf.savefig(plt.gcf(), bbox_inches='tight')
            plt.close()
            
            # Page 4: Scalabilité
            self.plot_scalability_analysis()
            pdf.savefig(plt.gcf(), bbox_inches='tight')
            plt.close()
            
            # Page 5: Conditions réseau
            self.plot_network_conditions_impact()
            pdf.savefig(plt.gcf(), bbox_inches='tight')
            plt.close()
            
        print("📄 Rapport PDF généré: quic_benchmark_report.pdf")

def main():
    parser = argparse.ArgumentParser(description='Analyseur de benchmark QUIC')
    parser.add_argument('--csv', default='benchmark_results.csv', 
                       help='Fichier CSV des résultats')
    parser.add_argument('--json', default='benchmark_results.json', 
                       help='Fichier JSON des résultats')
    parser.add_argument('--summary', action='store_true', 
                       help='Afficher seulement le résumé')
    parser.add_argument('--pdf', action='store_true', 
                       help='Générer le rapport PDF complet')
    
    args = parser.parse_args()
    
    # Initialisation de l'analyseur
    analyzer = QUICBenchmarkAnalyzer(args.csv, args.json)
    
    if analyzer.df is None:
        print("❌ Impossible de charger les données.")
        return
    
    print("🎯 Analyse des résultats du benchmark QUIC")
    print("="*60)
    
    # Génération des analyses
    analyzer.generate_summary_statistics()
    
    if not args.summary:
        print("\n📊 Génération des graphiques...")
        
        analyzer.plot_performance_overview()
        analyzer.plot_latency_analysis()
        analyzer.plot_throughput_analysis()
        analyzer.plot_scalability_analysis()
        analyzer.plot_network_conditions_impact()
        
        analyzer.generate_comparative_analysis()
        
        if args.pdf:
            print("\n📄 Génération du rapport PDF...")
            analyzer.generate_detailed_report()
    
    print("\n✅ Analyse terminée!")

if __name__ == "__main__":
    main()